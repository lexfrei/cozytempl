package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/lexfrei/cozytempl/internal/auth"
	"github.com/lexfrei/cozytempl/internal/k8s"
)

// paletteFanoutConcurrency bounds the number of concurrent per-
// tenant HelmRelease list calls the palette-index handler fans
// out. Picked high enough that a 20-tenant deployment completes
// in ~one apiserver round-trip worth of latency, low enough that
// a misconfigured cluster with hundreds of tenants doesn't bury
// the apiserver under a thundering herd from a single palette
// open. Tune by measuring p99 palette latency vs apiserver CPU.
const paletteFanoutConcurrency = 8

// PaletteHandler serves the command-palette search index. The
// client-side palette (static/ts/palette.ts) fetches this once per
// open() to populate dynamic entries — tenants + applications the
// caller has RBAC to see — on top of the static navigation /
// language / theme catalogue.
//
// The index is returned whole rather than as a server-side search
// endpoint. Rationale: the list is bounded by the caller's RBAC
// (typically a handful of tenants and tens of apps per operator),
// client-side substring match is instant, and a single fetch per
// palette open avoids the per-keystroke round-trip that a
// server-side search would require.
type PaletteHandler struct {
	tenants paletteTenantLister
	apps    paletteAppLister
	log     *slog.Logger
}

// paletteTenantLister is the narrow slice of TenantService the
// handler uses. ListMinimal (not the heavier List) is chosen
// deliberately: the palette never reads AppCount / ChildCount,
// and List's per-tenant counter fan-out would double the
// apiserver round-trip cost on the request hot path.
type paletteTenantLister interface {
	ListMinimal(ctx context.Context, usr *auth.UserContext) ([]k8s.Tenant, error)
}

// paletteAppLister mirrors the slice of ApplicationService the
// handler actually calls. Same test-seam motivation as
// paletteTenantLister.
type paletteAppLister interface {
	List(ctx context.Context, usr *auth.UserContext, tenant string) (k8s.ApplicationList, error)
}

// NewPaletteHandler wires the palette handler. Takes both listers
// because the index needs both — splitting into two endpoints
// would just double the round-trip without simplifying either
// call site.
func NewPaletteHandler(
	tenants paletteTenantLister, apps paletteAppLister, log *slog.Logger,
) *PaletteHandler {
	return &PaletteHandler{tenants: tenants, apps: apps, log: log}
}

// PaletteIndex is the JSON payload the client consumes. Kept in
// one place so the TypeScript side can mirror the shape exactly
// (static/ts/palette.ts PaletteIndex interface).
//
// JSON field names are effectively an API contract with the
// TypeScript side — rename one here and `static/ts/palette.ts`
// starts reading `undefined` silently. The struct tags below
// are the single source of truth; TestPaletteIndexShape pins
// them against the exact strings the client reads.
type PaletteIndex struct {
	Tenants []PaletteTenant `json:"tenants"`
	Apps    []PaletteApp    `json:"apps"`
	// TruncatedTenants lists namespaces whose app list hit the
	// AppListLimit cap. The palette renders a muted notice when
	// this list is non-empty so an operator with 500+ apps in
	// one tenant knows why app #501 is not searchable without
	// running a kubectl list directly.
	TruncatedTenants []string `json:"truncatedTenants,omitempty"`
}

// PaletteTenant is the minimum the client needs to render a "Go
// to tenant X" row. DisplayName mirrors the sidebar's rendering
// so a user sees the same label in the palette they see in the
// tree.
type PaletteTenant struct {
	Namespace   string `json:"namespace"`
	DisplayName string `json:"displayName"`
}

// PaletteApp carries just enough to render and navigate: the
// tenant it lives in, the app name, and its Kind so the palette
// can show the kind as a secondary hint ("my-postgres · Postgres
// · tenant-prod"). No status, no timestamps — the palette is a
// navigator, not a dashboard.
type PaletteApp struct {
	Name   string `json:"name"`
	Tenant string `json:"tenant"`
	Kind   string `json:"kind"`
}

// Index serves GET /api/palette-index. Fans out one application
// List per visible tenant using a bounded errgroup so a 20-tenant
// deployment completes in roughly one apiserver round-trip worth
// of latency instead of N serial round-trips.
//
// Per-tenant List errors are logged but do not abort the response
// — a single broken tenant should not blank the palette. Context
// cancellation short-circuits the loop so a client disconnect
// does not emit one Warn line per remaining tenant.
func (plh *PaletteHandler) Index(writer http.ResponseWriter, req *http.Request) {
	usr := auth.UserFromContext(req.Context())
	if usr == nil {
		Error(writer, http.StatusUnauthorized, "not authenticated")

		return
	}

	tenants, err := plh.tenants.ListMinimal(req.Context(), usr)
	if err != nil {
		plh.log.Error("listing tenants for palette", "error", err)
		Error(writer, http.StatusInternalServerError, "failed to list tenants")

		return
	}

	index := PaletteIndex{
		Tenants: make([]PaletteTenant, 0, len(tenants)),
		Apps:    make([]PaletteApp, 0),
	}

	for idx := range tenants {
		index.Tenants = append(index.Tenants, PaletteTenant{
			Namespace:   tenants[idx].Namespace,
			DisplayName: tenants[idx].DisplayName,
		})
	}

	plh.fanoutAppLists(req.Context(), usr, tenants, &index)

	// Fan-out appends apps in completion order, not tenant order.
	// Sorting by (tenant, name) gives the palette a deterministic
	// display across opens — an operator who scans top-to-bottom
	// sees the same layout every time, and tests can pin order
	// without threading execution determinism through the workers.
	sort.Slice(index.Apps, func(i, j int) bool {
		if index.Apps[i].Tenant != index.Apps[j].Tenant {
			return index.Apps[i].Tenant < index.Apps[j].Tenant
		}

		return index.Apps[i].Name < index.Apps[j].Name
	})

	sort.Strings(index.TruncatedTenants)

	JSON(writer, http.StatusOK, index)
}

// fanoutAppLists fires per-tenant app list calls through a
// bounded errgroup and folds the results into index. A mutex
// serialises the slice appends; the critical section is tiny
// (two appends per tenant at most) so contention is negligible
// even at paletteFanoutConcurrency workers.
func (plh *PaletteHandler) fanoutAppLists(
	ctx context.Context, usr *auth.UserContext, tenants []k8s.Tenant, index *PaletteIndex,
) {
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(paletteFanoutConcurrency)

	var mu sync.Mutex

	for idx := range tenants {
		tenant := tenants[idx].Namespace

		group.Go(func() error {
			// Bail on request context cancellation so a 50-tenant
			// palette does not emit 50 warn lines when the
			// client disconnects mid-fan-out.
			if groupCtx.Err() != nil {
				return nil
			}

			appList, listErr := plh.apps.List(groupCtx, usr, tenant)
			if listErr != nil {
				plh.logTenantListErr(tenant, listErr)

				return nil
			}

			mu.Lock()
			defer mu.Unlock()

			if appList.Truncated {
				plh.log.Warn("palette index truncated for tenant",
					"tenant", tenant, "limit", k8s.AppListLimit)

				index.TruncatedTenants = append(index.TruncatedTenants, tenant)
			}

			for i := range appList.Items {
				index.Apps = append(index.Apps, PaletteApp{
					Name:   appList.Items[i].Name,
					Tenant: tenant,
					Kind:   appList.Items[i].Kind,
				})
			}

			return nil
		})
	}

	_ = group.Wait() // individual errors are already logged; fanoutAppLists never returns one.
}

// logTenantListErr separates context-cancel errors from real
// apiserver failures so a client disconnect does not look like a
// swarm of tenant-level problems in the log. Context errors drop
// silently; anything else gets a Warn line with the tenant name
// so operators can triage.
func (plh *PaletteHandler) logTenantListErr(tenant string, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}

	plh.log.Warn("listing apps for palette", "tenant", tenant, "error", err)
}

package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/lexfrei/cozytempl/internal/auth"
	"github.com/lexfrei/cozytempl/internal/k8s"
)

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
// handler uses. Split out so unit tests can inject a stub without
// standing up the concrete k8s client — the real service
// satisfies it by construction.
type paletteTenantLister interface {
	List(ctx context.Context, usr *auth.UserContext) ([]k8s.Tenant, error)
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
type PaletteIndex struct {
	Tenants []PaletteTenant `json:"tenants"`
	Apps    []PaletteApp    `json:"apps"`
	// TruncatedTenants lists namespaces whose app list hit the
	// 500-entry cap (see k8s.AppListLimit). The client can render
	// a hint so an operator who fails to find app #501 knows
	// why; without surfacing the bit, the palette would silently
	// skip apps past the limit.
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
// List per visible tenant (same pattern as the dashboard and
// overview page) and assembles the {tenants, apps} payload.
//
// Per-tenant List errors are logged but do not abort the response
// — a single broken tenant should not blank the palette. The
// client gets whatever succeeded. If a tenant's list was
// truncated by the server-side cap, its namespace is reported in
// TruncatedTenants so the UI can surface the limitation.
func (plh *PaletteHandler) Index(writer http.ResponseWriter, req *http.Request) {
	usr := auth.UserFromContext(req.Context())
	if usr == nil {
		Error(writer, http.StatusUnauthorized, "not authenticated")

		return
	}

	tenants, err := plh.tenants.List(req.Context(), usr)
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

		plh.appendTenantApps(req.Context(), usr, tenants[idx].Namespace, &index)
	}

	JSON(writer, http.StatusOK, index)
}

// appendTenantApps lists one tenant's apps and folds them into
// the shared index. Split from Index so the main handler stays
// under the funlen budget and the per-tenant error / truncation
// branches can be read in isolation.
func (plh *PaletteHandler) appendTenantApps(
	ctx context.Context, usr *auth.UserContext, tenant string, index *PaletteIndex,
) {
	appList, err := plh.apps.List(ctx, usr, tenant)
	if err != nil {
		plh.log.Warn("listing apps for palette", "tenant", tenant, "error", err)

		return
	}

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
}

package api

import (
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
	tenantSvc *k8s.TenantService
	appSvc    *k8s.ApplicationService
	log       *slog.Logger
}

// NewPaletteHandler wires the palette handler. Takes both the
// tenant and application services because the index needs both —
// splitting into two endpoints would just double the round-trip
// without simplifying either call site.
func NewPaletteHandler(
	tenantSvc *k8s.TenantService, appSvc *k8s.ApplicationService, log *slog.Logger,
) *PaletteHandler {
	return &PaletteHandler{tenantSvc: tenantSvc, appSvc: appSvc, log: log}
}

// PaletteIndex is the JSON payload the client consumes. Kept in
// one place so the TypeScript side can mirror the shape exactly
// (static/ts/palette.ts PaletteIndex interface).
type PaletteIndex struct {
	Tenants []PaletteTenant `json:"tenants"`
	Apps    []PaletteApp    `json:"apps"`
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
// client gets whatever succeeded.
func (plh *PaletteHandler) Index(writer http.ResponseWriter, req *http.Request) {
	usr := auth.UserFromContext(req.Context())
	if usr == nil {
		Error(writer, http.StatusUnauthorized, "not authenticated")

		return
	}

	tenants, err := plh.tenantSvc.List(req.Context(), usr)
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

		appList, listErr := plh.appSvc.List(req.Context(), usr, tenants[idx].Namespace)
		if listErr != nil {
			plh.log.Warn("listing apps for palette",
				"tenant", tenants[idx].Namespace, "error", listErr)

			continue
		}

		for i := range appList.Items {
			index.Apps = append(index.Apps, PaletteApp{
				Name:   appList.Items[i].Name,
				Tenant: tenants[idx].Namespace,
				Kind:   appList.Items[i].Kind,
			})
		}
	}

	JSON(writer, http.StatusOK, index)
}

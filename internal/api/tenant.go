package api

import (
	"log/slog"
	"net/http"

	"github.com/lexfrei/cozytempl/internal/auth"
	"github.com/lexfrei/cozytempl/internal/k8s"
)

// TenantHandler handles tenant API requests.
type TenantHandler struct {
	svc *k8s.TenantService
	log *slog.Logger
}

// NewTenantHandler creates a new tenant handler.
func NewTenantHandler(svc *k8s.TenantService, log *slog.Logger) *TenantHandler {
	return &TenantHandler{svc: svc, log: log}
}

// List returns all tenants.
func (tnh *TenantHandler) List(writer http.ResponseWriter, req *http.Request) {
	usr := auth.UserFromContext(req.Context())
	if usr == nil {
		Error(writer, http.StatusUnauthorized, "not authenticated")

		return
	}

	tenants, err := tnh.svc.List(req.Context(), usr.Username, usr.Groups)
	if err != nil {
		tnh.log.Error("listing tenants", "error", err)
		Error(writer, http.StatusInternalServerError, "failed to list tenants")

		return
	}

	JSON(writer, http.StatusOK, tenants)
}

// Get returns a single tenant by name.
func (tnh *TenantHandler) Get(writer http.ResponseWriter, req *http.Request) {
	usr := auth.UserFromContext(req.Context())
	if usr == nil {
		Error(writer, http.StatusUnauthorized, "not authenticated")

		return
	}

	name := req.PathValue("name")

	tenant, err := tnh.svc.Get(req.Context(), usr.Username, usr.Groups, name)
	if err != nil {
		tnh.log.Error("getting tenant", "name", name, "error", err)
		Error(writer, http.StatusInternalServerError, "failed to get tenant")

		return
	}

	JSON(writer, http.StatusOK, tenant)
}

// Create creates a new tenant.
func (tnh *TenantHandler) Create(writer http.ResponseWriter, req *http.Request) {
	usr := auth.UserFromContext(req.Context())
	if usr == nil {
		Error(writer, http.StatusUnauthorized, "not authenticated")

		return
	}

	var body k8s.CreateTenantRequest

	err := DecodeJSON(writer, req, &body)
	if err != nil {
		Error(writer, http.StatusBadRequest, "invalid request body")

		return
	}

	tenant, err := tnh.svc.Create(req.Context(), usr.Username, usr.Groups, body)
	if err != nil {
		tnh.log.Error("creating tenant", "error", err)
		Error(writer, http.StatusInternalServerError, "failed to create tenant")

		return
	}

	JSON(writer, http.StatusCreated, tenant)
}

// Delete removes a tenant.
func (tnh *TenantHandler) Delete(writer http.ResponseWriter, req *http.Request) {
	usr := auth.UserFromContext(req.Context())
	if usr == nil {
		Error(writer, http.StatusUnauthorized, "not authenticated")

		return
	}

	name := req.PathValue("name")
	namespace := req.URL.Query().Get("namespace")

	err := tnh.svc.Delete(req.Context(), usr.Username, usr.Groups, namespace, name)
	if err != nil {
		tnh.log.Error("deleting tenant", "namespace", namespace, "name", name, "error", err)
		Error(writer, http.StatusInternalServerError, "failed to delete tenant")

		return
	}

	writer.WriteHeader(http.StatusNoContent)
}

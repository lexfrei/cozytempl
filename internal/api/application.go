package api

import (
	"log/slog"
	"net/http"

	"github.com/lexfrei/cozytempl/internal/auth"
	"github.com/lexfrei/cozytempl/internal/k8s"
)

// ApplicationHandler handles application API requests.
type ApplicationHandler struct {
	svc *k8s.ApplicationService
	log *slog.Logger
}

// NewApplicationHandler creates a new application handler.
func NewApplicationHandler(svc *k8s.ApplicationService, log *slog.Logger) *ApplicationHandler {
	return &ApplicationHandler{svc: svc, log: log}
}

// List returns all applications in a tenant.
func (aph *ApplicationHandler) List(writer http.ResponseWriter, req *http.Request) {
	usr := auth.UserFromContext(req.Context())
	if usr == nil {
		Error(writer, http.StatusUnauthorized, "not authenticated")

		return
	}

	tenant := req.PathValue("tenant")

	apps, err := aph.svc.List(req.Context(), usr.Username, usr.Groups, tenant)
	if err != nil {
		aph.log.Error("listing apps", "tenant", tenant, "error", err)
		Error(writer, http.StatusInternalServerError, "failed to list applications")

		return
	}

	JSON(writer, http.StatusOK, apps)
}

// Get returns a single application.
func (aph *ApplicationHandler) Get(writer http.ResponseWriter, req *http.Request) {
	usr := auth.UserFromContext(req.Context())
	if usr == nil {
		Error(writer, http.StatusUnauthorized, "not authenticated")

		return
	}

	tenant := req.PathValue("tenant")
	name := req.PathValue("name")

	application, err := aph.svc.Get(req.Context(), usr.Username, usr.Groups, tenant, name)
	if err != nil {
		aph.log.Error("getting app", "tenant", tenant, "name", name, "error", err)
		Error(writer, http.StatusInternalServerError, "failed to get application")

		return
	}

	JSON(writer, http.StatusOK, application)
}

// Create creates a new application.
func (aph *ApplicationHandler) Create(writer http.ResponseWriter, req *http.Request) {
	usr := auth.UserFromContext(req.Context())
	if usr == nil {
		Error(writer, http.StatusUnauthorized, "not authenticated")

		return
	}

	tenant := req.PathValue("tenant")

	var body k8s.CreateApplicationRequest

	err := DecodeJSON(req, &body)
	if err != nil {
		Error(writer, http.StatusBadRequest, "invalid request body")

		return
	}

	application, err := aph.svc.Create(req.Context(), usr.Username, usr.Groups, tenant, body)
	if err != nil {
		aph.log.Error("creating app", "tenant", tenant, "error", err)
		Error(writer, http.StatusInternalServerError, "failed to create application")

		return
	}

	JSON(writer, http.StatusCreated, application)
}

// Update updates an application's values.
func (aph *ApplicationHandler) Update(writer http.ResponseWriter, req *http.Request) {
	usr := auth.UserFromContext(req.Context())
	if usr == nil {
		Error(writer, http.StatusUnauthorized, "not authenticated")

		return
	}

	tenant := req.PathValue("tenant")
	name := req.PathValue("name")

	var body k8s.UpdateApplicationRequest

	err := DecodeJSON(req, &body)
	if err != nil {
		Error(writer, http.StatusBadRequest, "invalid request body")

		return
	}

	application, err := aph.svc.Update(req.Context(), usr.Username, usr.Groups, tenant, name, body)
	if err != nil {
		aph.log.Error("updating app", "tenant", tenant, "name", name, "error", err)
		Error(writer, http.StatusInternalServerError, "failed to update application")

		return
	}

	JSON(writer, http.StatusOK, application)
}

// Delete removes an application.
func (aph *ApplicationHandler) Delete(writer http.ResponseWriter, req *http.Request) {
	usr := auth.UserFromContext(req.Context())
	if usr == nil {
		Error(writer, http.StatusUnauthorized, "not authenticated")

		return
	}

	tenant := req.PathValue("tenant")
	name := req.PathValue("name")

	err := aph.svc.Delete(req.Context(), usr.Username, usr.Groups, tenant, name)
	if err != nil {
		aph.log.Error("deleting app", "tenant", tenant, "name", name, "error", err)
		Error(writer, http.StatusInternalServerError, "failed to delete application")

		return
	}

	writer.WriteHeader(http.StatusNoContent)
}

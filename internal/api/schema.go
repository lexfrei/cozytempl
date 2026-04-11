package api

import (
	"log/slog"
	"net/http"

	"github.com/lexfrei/cozytempl/internal/auth"
	"github.com/lexfrei/cozytempl/internal/k8s"
)

// SchemaHandler handles schema API requests.
type SchemaHandler struct {
	svc *k8s.SchemaService
	log *slog.Logger
}

// NewSchemaHandler creates a new schema handler.
func NewSchemaHandler(svc *k8s.SchemaService, log *slog.Logger) *SchemaHandler {
	return &SchemaHandler{svc: svc, log: log}
}

// List returns all available application schemas.
func (sch *SchemaHandler) List(writer http.ResponseWriter, req *http.Request) {
	usr := auth.UserFromContext(req.Context())
	if usr == nil {
		Error(writer, http.StatusUnauthorized, "not authenticated")

		return
	}

	schemas, err := sch.svc.List(req.Context(), usr)
	if err != nil {
		sch.log.Error("listing schemas", "error", err)
		Error(writer, http.StatusInternalServerError, "failed to list schemas")

		return
	}

	JSON(writer, http.StatusOK, schemas)
}

// Get returns the schema for a specific application kind.
func (sch *SchemaHandler) Get(writer http.ResponseWriter, req *http.Request) {
	usr := auth.UserFromContext(req.Context())
	if usr == nil {
		Error(writer, http.StatusUnauthorized, "not authenticated")

		return
	}

	kind := req.PathValue("kind")

	schema, err := sch.svc.Get(req.Context(), usr, kind)
	if err != nil {
		sch.log.Error("getting schema", "kind", kind, "error", err)
		Error(writer, http.StatusInternalServerError, "failed to get schema")

		return
	}

	JSON(writer, http.StatusOK, schema)
}

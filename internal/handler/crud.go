package handler

import (
	"net/http"

	"github.com/lexfrei/cozytempl/internal/auth"
	"github.com/lexfrei/cozytempl/internal/k8s"
	"github.com/lexfrei/cozytempl/internal/view/partial"
)

const (
	maxFormBytes  = 1 << 20 // 1 MB
	formFieldName = "name"
	formFieldKind = "kind"
	sortByName    = "name"
	sortByKind    = "kind"
)

// CreateApp handles POST to create a new application.
func (pgh *PageHandler) CreateApp(writer http.ResponseWriter, req *http.Request) {
	usr := auth.UserFromContext(req.Context())
	if usr == nil {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)

		return
	}

	tenantNS := req.PathValue("tenant")

	req.Body = http.MaxBytesReader(writer, req.Body, maxFormBytes)

	parseErr := req.ParseForm()
	if parseErr != nil {
		http.Error(writer, "bad request", http.StatusBadRequest)

		return
	}

	appName := req.FormValue(formFieldName)
	appKind := req.FormValue(formFieldKind)

	if appName == "" || appKind == "" {
		http.Error(writer, "name and kind required", http.StatusBadRequest)

		return
	}

	pgh.doCreateApp(writer, req, usr, tenantNS, appName, appKind)
}

func (pgh *PageHandler) doCreateApp(
	writer http.ResponseWriter,
	req *http.Request,
	usr *auth.UserContext,
	tenantNS, appName, appKind string,
) {
	createReq := k8s.CreateApplicationRequest{
		Name: appName,
		Kind: appKind,
		Spec: extractSpecFromForm(req),
	}

	_, err := pgh.appSvc.Create(req.Context(), usr.Username, usr.Groups, tenantNS, createReq)
	if err != nil {
		pgh.log.Error("creating app", "tenant", tenantNS, "name", appName, "error", err)
		pgh.renderToast(writer, req, "error", "Failed to create "+appName+": "+err.Error())

		return
	}

	pgh.log.Info("app created", "tenant", tenantNS, "name", appName, "kind", appKind)

	writer.Header().Set("Hx-Redirect", "/tenants/"+tenantNS)
	writer.WriteHeader(http.StatusCreated)
}

// DeleteApp handles DELETE to remove an application.
func (pgh *PageHandler) DeleteApp(writer http.ResponseWriter, req *http.Request) {
	usr := auth.UserFromContext(req.Context())
	if usr == nil {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)

		return
	}

	tenantNS := req.PathValue("tenant")
	appName := req.PathValue("name")

	err := pgh.appSvc.Delete(req.Context(), usr.Username, usr.Groups, tenantNS, appName)
	if err != nil {
		pgh.log.Error("deleting app", "tenant", tenantNS, "name", appName, "error", err)
		pgh.renderToast(writer, req, "error", "Failed to delete "+appName)

		return
	}

	pgh.log.Info("app deleted", "tenant", tenantNS, "name", appName)

	writer.WriteHeader(http.StatusOK)
}

func (pgh *PageHandler) renderToast(writer http.ResponseWriter, req *http.Request, toastType, msg string) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")

	renderErr := partial.Toast(toastType, msg).Render(req.Context(), writer)
	if renderErr != nil {
		pgh.log.Error("rendering toast", "error", renderErr)
	}
}

func extractSpecFromForm(req *http.Request) map[string]any {
	spec := map[string]any{}

	for key, values := range req.Form {
		if key == formFieldName || key == formFieldKind {
			continue
		}

		if len(values) > 0 && values[0] != "" {
			spec[key] = values[0]
		}
	}

	if len(spec) == 0 {
		return nil
	}

	return spec
}

package handler

import (
	"net/http"
	"sort"
	"strings"

	"github.com/lexfrei/cozytempl/internal/auth"
	"github.com/lexfrei/cozytempl/internal/k8s"
	"github.com/lexfrei/cozytempl/internal/view/page"
)

// AppTableFragment renders filtered app table rows for htmx swap.
func (pgh *PageHandler) AppTableFragment(writer http.ResponseWriter, req *http.Request) {
	usr := auth.UserFromContext(req.Context())
	if usr == nil {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)

		return
	}

	tenant := req.URL.Query().Get("tenant")

	apps, err := pgh.appSvc.List(req.Context(), usr.Username, usr.Groups, tenant)
	if err != nil {
		pgh.log.Error("listing apps for fragment", "tenant", tenant, "error", err)

		apps = []k8s.Application{}
	}

	apps = filterAndSortApps(
		apps,
		req.URL.Query().Get("q"),
		req.URL.Query().Get("kind"),
		req.URL.Query().Get("sort"),
	)

	writer.Header().Set("Content-Type", "text/html; charset=utf-8")

	renderErr := page.AppTableRows(tenant, apps).Render(req.Context(), writer)
	if renderErr != nil {
		pgh.log.Error("rendering app table fragment", "error", renderErr)
	}
}

// MarketplaceFragment renders filtered marketplace grid for htmx swap.
func (pgh *PageHandler) MarketplaceFragment(writer http.ResponseWriter, req *http.Request) {
	usr := auth.UserFromContext(req.Context())
	if usr == nil {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)

		return
	}

	schemas, _ := pgh.schemaSvc.List(req.Context(), usr.Username, usr.Groups)

	data := buildMarketplaceData(
		schemas,
		req.URL.Query().Get("q"),
		req.URL.Query().Get("category"),
		req.URL.Query().Get("tag"),
	)

	writer.Header().Set("Content-Type", "text/html; charset=utf-8")

	renderErr := page.MarketplaceGrid(data).Render(req.Context(), writer)
	if renderErr != nil {
		pgh.log.Error("rendering marketplace fragment", "error", renderErr)
	}
}

func filterAndSortApps(apps []k8s.Application, query, kindFilter, sortBy string) []k8s.Application {
	var filtered []k8s.Application

	for idx := range apps {
		if kindFilter != "" && apps[idx].Kind != kindFilter {
			continue
		}

		if query != "" {
			q := strings.ToLower(query)
			if !strings.Contains(strings.ToLower(apps[idx].Name), q) {
				continue
			}
		}

		filtered = append(filtered, apps[idx])
	}

	if sortBy == "" {
		sortBy = sortByName
	}

	sort.Slice(filtered, func(left, right int) bool {
		switch sortBy {
		case sortByKind:
			return filtered[left].Kind < filtered[right].Kind
		case "status":
			return string(filtered[left].Status) < string(filtered[right].Status)
		default:
			return filtered[left].Name < filtered[right].Name
		}
	})

	return filtered
}

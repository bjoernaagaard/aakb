// Package httpapi serves the aakb HTTP resources and protects shared routes.
package httpapi

import (
	"log/slog"
	"net/http"
	"net/netip"
	"strings"

	"github.com/bjoernaagaard/aakb"
)

const basePath = "/api/v1"
const downloadsPath = "/downloads/"

// Options configures HTTP access and the public server version.
type Options struct {
	Token   string
	Addr    string
	Version string
	// TrustedProxies lists the networks whose X-Forwarded-For header
	// names the client for rate limiting.
	TrustedProxies []netip.Prefix
}

// API serves the REST resources and completed files.
type API struct {
	client  *aakb.Client
	version string
	logger  *slog.Logger
	guard   *accessGuard
	routes  map[string]route
}

type route struct {
	method  string
	handler http.HandlerFunc
}

// New creates an API and the guard shared by its routes and MCP.
func New(client *aakb.Client, opts Options, logger *slog.Logger) *API {
	api := &API{client: client, version: opts.Version, logger: logger, guard: newGuard(opts)}
	api.routes = map[string]route{
		basePath + "/capabilities":   {http.MethodGet, api.capabilities},
		basePath + "/search":         {http.MethodGet, api.search},
		basePath + "/records/lookup": {http.MethodGet, api.lookup},
		basePath + "/account":        {http.MethodGet, api.account},
		basePath + "/article-reads":  {http.MethodPost, api.readArticle},
		basePath + "/downloads":      {http.MethodPost, api.download},
	}
	if opts.Token == "" && !api.guard.loopback {
		logger.Warn("REST API requires AAKB_HTTP_TOKEN on a non-loopback listener")
	}
	return api
}

// Routes serves REST and downloads, delegating other requests to fallback.
func (a *API) Routes(fallback http.Handler) http.Handler {
	rest := a.guard.wrap(http.HandlerFunc(a.serveREST), restRoute)
	files := a.guard.wrap(http.HandlerFunc(a.serveFile), fileRoute)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case isREST(r):
			rest.ServeHTTP(w, r)
		case strings.HasPrefix(r.URL.Path, downloadsPath):
			files.ServeHTTP(w, r)
		default:
			fallback.ServeHTTP(w, r)
		}
	})
}

// Guard protects MCP while preserving its plain-text error responses.
func (a *API) Guard(next http.Handler) http.Handler {
	return a.guard.wrap(next, mcpRoute)
}

// PublicUI protects the UI on tokenless listeners without limiting or authenticating assets.
func (a *API) PublicUI(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(a.guard.bearer) == 0 && !a.guard.allowTokenless(w, r) {
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Protect rejects cross-origin writes, preserving MCP's existing error format.
func Protect(next http.Handler) http.Handler {
	protection := http.NewCrossOriginProtection()
	protection.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isREST(r) {
			writeProblem(w, http.StatusForbidden, string(aakb.CodeInvalidArgument), "Cross-origin requests are forbidden.")
			return
		}
		// Use the standard response (including its reason) for existing routes.
		http.Error(w, protection.Check(r).Error(), http.StatusForbidden)
	}))
	return protection.Handler(next)
}

func isREST(r *http.Request) bool {
	return r.URL.Path == basePath || strings.HasPrefix(r.URL.Path, basePath+"/")
}

func (a *API) serveREST(w http.ResponseWriter, r *http.Request) {
	endpoint, ok := a.routes[r.URL.Path]
	if !ok {
		writeProblem(w, http.StatusNotFound, "ROUTE_NOT_FOUND", "This API route does not exist.")
		return
	}
	if !allowMethod(w, r, endpoint.method) {
		return
	}
	endpoint.handler(w, r)
}

func allowMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method || (method == http.MethodGet && r.Method == http.MethodHead) {
		return true
	}
	allow := method
	if method == http.MethodGet {
		allow += ", " + http.MethodHead
	}
	w.Header().Set("Allow", allow)
	writeProblem(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "This method is unavailable for the resource.")
	return false
}

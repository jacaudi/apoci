package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
)

// openapiDoc is the minimal shape the served spec must expose. Method keys under
// each path are lowercased per the OpenAPI spec (get, post, patch, delete).
type openapiDoc struct {
	OpenAPI string                                `json:"openapi"`
	Info    map[string]any                        `json:"info"`
	Paths   map[string]map[string]json.RawMessage `json:"paths"`
}

func getSpec(t *testing.T, srvURL string) openapiDoc {
	t.Helper()
	req, _ := http.NewRequest("GET", srvURL+"/api/admin/openapi.json", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "application/json", resp.Header.Get("Content-Type"))

	var doc openapiDoc
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&doc), "served spec must be valid JSON")
	return doc
}

func TestAdminOpenAPISpecServed(t *testing.T) {
	s := testServer(t)
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	doc := getSpec(t, srv.URL)

	require.NotEmpty(t, doc.OpenAPI, "spec must declare an openapi version")
	require.NotEmpty(t, doc.Info, "spec must have an info block")
	require.NotEmpty(t, doc.Paths, "spec must have paths")
}

func TestAdminOpenAPISpecRequiresAuth(t *testing.T) {
	s := testServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/admin/openapi.json")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

// specPath converts a chi route pattern to the path form used in the OpenAPI
// document. The only non-literal admin route is the mirror wildcard.
func specPath(chiPattern string) string {
	if chiPattern == "/mirrors/*" {
		return "/mirrors/{repository}"
	}
	return chiPattern
}

// TestAdminOpenAPISpecCoversAllAdminRoutes walks the live admin router and
// asserts every registered route (except the spec route itself) is documented
// in the served OpenAPI document. This fails if a route is added to admin.go
// without being added to openapi.json.
func TestAdminOpenAPISpecCoversAllAdminRoutes(t *testing.T) {
	s := testServer(t)
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	doc := getSpec(t, srv.URL)

	router, ok := s.adminRouter().(chi.Routes)
	require.True(t, ok, "adminRouter must expose chi.Routes for walking")

	var routeCount int
	err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if route == "/openapi.json" {
			return nil
		}
		routeCount++
		path := specPath(route)
		ops, found := doc.Paths[path]
		require.Truef(t, found, "route %s %s missing from spec paths", method, path)
		_, hasMethod := ops[strings.ToLower(method)]
		require.Truef(t, hasMethod, "route %s %s: method not documented in spec", method, path)
		return nil
	})
	require.NoError(t, err)

	// The 18 documented admin routes (openapi.json is excluded above).
	require.Equal(t, 18, routeCount, "expected 18 admin routes to be documented")
}

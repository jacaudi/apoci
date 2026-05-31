package replication

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// tokenAuthServer emulates a registry that requires a bearer token obtained via
// the Docker v2 token flow. It records token-endpoint calls and the last scope.
type tokenAuthServer struct {
	srv        *httptest.Server
	tokenCalls atomic.Int64
	lastScope  atomic.Value // string
	lastBasic  atomic.Value // string username seen at the token endpoint
}

func newTokenAuthServer(t *testing.T) *tokenAuthServer {
	t.Helper()
	s := &tokenAuthServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		s.tokenCalls.Add(1)
		s.lastScope.Store(r.URL.Query().Get("scope"))
		if u, _, ok := r.BasicAuth(); ok {
			s.lastBasic.Store(u)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "test-token", "expires_in": 300})
	})
	mux.HandleFunc(testV2Root, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == testV2Root {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="myreg"`, s.srv.URL))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK) // authenticated: pretend the blob exists
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func TestClientTokenAuthFlow(t *testing.T) {
	s := newTokenAuthServer(t)
	c := NewClient(Target{Endpoint: s.srv.URL, Auth: authToken, Username: testUser, Password: testPass}, 0)

	exists, err := c.BlobExists(context.Background(), "org/app", "sha256:abc")
	require.NoError(t, err)
	require.True(t, exists, "authenticated request should reach the registry")

	require.Equal(t, int64(1), s.tokenCalls.Load())
	require.Equal(t, "repository:org/app:pull,push", s.lastScope.Load())
	require.Equal(t, testUser, s.lastBasic.Load(), "credentials should be sent to the token endpoint")

	// A second op on the same repo reuses the cached token.
	_, err = c.BlobExists(context.Background(), "org/app", "sha256:def")
	require.NoError(t, err)
	require.Equal(t, int64(1), s.tokenCalls.Load(), "token should be cached per repo")
}

func TestClientBasicAuthFlow(t *testing.T) {
	var sawUser atomic.Value
	mux := http.NewServeMux()
	mux.HandleFunc(testV2Root, func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != testUser || p != testPass {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		sawUser.Store(u)
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(Target{Endpoint: srv.URL, Auth: authBasic, Username: testUser, Password: testPass}, 0)
	exists, err := c.BlobExists(context.Background(), "org/app", "sha256:abc")
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, testUser, sawUser.Load())
}

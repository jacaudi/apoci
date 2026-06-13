package goproxy

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/blobstore"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/upstream"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/validate"
)

const (
	testToken    = "test-token"
	testOwnerURL = "https://alice.example.com/ap/actor"
	testModule   = "github.com/Example/Widget"
	testVersion  = "v1.0.0"
)

func nopLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestServer(t *testing.T) *httptest.Server {
	return newBackend(t, nil)
}

func newBackend(t *testing.T, up *upstream.GoFetcher) *httptest.Server {
	t.Helper()
	db, err := database.OpenSQLite(t.TempDir(), 0, 0, nopLog())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	blobs, err := blobstore.New(t.TempDir(), nopLog())
	require.NoError(t, err)

	srv := httptest.NewServer(nil)
	t.Cleanup(srv.Close)

	b := New(Config{
		DB:       db,
		Blobs:    blobs,
		Endpoint: srv.URL,
		Token:    testToken,
		Owner:    testOwnerURL,
		Upstream: up,
		Logger:   nopLog(),
	})
	srv.Config.Handler = b.Handler()
	return srv
}

// buildModuleZip produces a minimal valid module zip: every entry is prefixed
// "<module>@<version>/", with a go.mod declaring the module path.
func buildModuleZip(t *testing.T, mod, ver string) []byte {
	t.Helper()
	prefix := mod + "@" + ver + "/"
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	gm, err := zw.Create(prefix + "go.mod")
	require.NoError(t, err)
	_, err = gm.Write([]byte("module " + mod + "\n\ngo 1.22\n"))
	require.NoError(t, err)
	src, err := zw.Create(prefix + "widget.go")
	require.NoError(t, err)
	_, err = src.Write([]byte("package widget\n"))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

// escaped is the bang-encoded module path used in proxy URLs.
const escapedModule = "github.com/!example/!widget"

func upload(t *testing.T, srv *httptest.Server, withAuth bool) *http.Response {
	t.Helper()
	zipBytes := buildModuleZip(t, testModule, testVersion)
	req, err := http.NewRequest(http.MethodPut, srv.URL+"/goproxy/"+escapedModule+"/@v/"+testVersion+".zip", bytes.NewReader(zipBytes))
	require.NoError(t, err)
	if withAuth {
		req.Header.Set("Authorization", "Bearer "+testToken)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

func getBody(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get(url) //nolint:gosec,noctx // test
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	_ = resp.Body.Close()
	return resp, string(body)
}

func TestUploadRequiresAuth(t *testing.T) {
	srv := newTestServer(t)
	resp := upload(t, srv, false)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestUploadAndDownloadRoundTrip(t *testing.T) {
	srv := newTestServer(t)

	resp := upload(t, srv, true)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	// list
	resp, body := getBody(t, srv.URL+"/goproxy/"+escapedModule+"/@v/list")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, testVersion, strings.TrimSpace(body))

	// .info
	resp, body = getBody(t, srv.URL+"/goproxy/"+escapedModule+"/@v/"+testVersion+".info")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	var info modInfo
	require.NoError(t, json.Unmarshal([]byte(body), &info))
	assert.Equal(t, testVersion, info.Version)
	assert.NotEmpty(t, info.Time)

	// .mod
	resp, body = getBody(t, srv.URL+"/goproxy/"+escapedModule+"/@v/"+testVersion+".mod")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, body, "module "+testModule)

	// .zip
	resp, body = getBody(t, srv.URL+"/goproxy/"+escapedModule+"/@v/"+testVersion+".zip")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	zr, err := zip.NewReader(bytes.NewReader([]byte(body)), int64(len(body)))
	require.NoError(t, err)
	assert.NotEmpty(t, zr.File)
}

func TestUploadDuplicateConflicts(t *testing.T) {
	srv := newTestServer(t)
	resp := upload(t, srv, true)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	resp = upload(t, srv, true)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
}

func TestUploadRejectsMismatchedGoMod(t *testing.T) {
	srv := newTestServer(t)
	// zip whose go.mod declares a different module than the URL path.
	prefix := testModule + "@" + testVersion + "/"
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	gm, _ := zw.Create(prefix + "go.mod")
	_, _ = gm.Write([]byte("module example.com/wrong\n\ngo 1.22\n"))
	_ = zw.Close()

	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/goproxy/"+escapedModule+"/@v/"+testVersion+".zip", bytes.NewReader(buf.Bytes()))
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestUnknownModuleNotFound(t *testing.T) {
	srv := newTestServer(t)
	resp, _ := getBody(t, srv.URL+"/goproxy/"+escapedModule+"/@v/"+testVersion+".info")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// TestPullThroughCache verifies a local miss is fetched from the upstream proxy,
// stored, and that a second request is served without hitting the upstream again.
func TestPullThroughCache(t *testing.T) {
	prev := validate.AllowPrivateIPs.Load()
	validate.AllowPrivateIPs.Store(true)
	t.Cleanup(func() { validate.AllowPrivateIPs.Store(prev) })

	var hits int
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		base := "/" + escapedModule + "/@v/" + testVersion
		switch r.URL.Path {
		case base + ".info":
			_, _ = w.Write([]byte(`{"Version":"` + testVersion + `","Time":"2024-01-01T00:00:00Z"}`))
		case base + ".mod":
			_, _ = w.Write([]byte("module " + testModule + "\n\ngo 1.22\n"))
		case base + ".zip":
			_, _ = w.Write(buildModuleZip(t, testModule, testVersion))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstreamSrv.Close)

	fetcher := upstream.NewGoFetcher([]string{upstreamSrv.URL}, 10*time.Second, 50<<20)
	srv := newBackend(t, fetcher)

	// First .zip request: local miss → pull-through from upstream.
	resp, body := getBody(t, srv.URL+"/goproxy/"+escapedModule+"/@v/"+testVersion+".zip")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.NotEmpty(t, body)
	hitsAfterFirst := hits
	assert.Positive(t, hitsAfterFirst, "expected upstream to be queried on first fetch")

	// Second request for the same artifact is served locally (no new upstream hits).
	resp, _ = getBody(t, srv.URL+"/goproxy/"+escapedModule+"/@v/"+testVersion+".zip")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, hitsAfterFirst, hits, "second fetch should be served from local cache")
}

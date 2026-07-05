package pypi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/blobstore"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/upstream"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/validate"
)

func TestVersionFromFilename(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"foo_bar-1.0-py3-none-any.whl", "1.0", true},
		{"foo_bar-2.1.3.tar.gz", "2.1.3", true},
		{"foo_bar-0.9.zip", "0.9", true},
		{"garbage.whl", "", false},
		{"noversion", "", false},
	}
	for _, c := range cases {
		got, ok := versionFromFilename(c.in)
		assert.Equal(t, c.ok, ok, "versionFromFilename(%q) ok", c.in)
		assert.Equal(t, c.want, got, "versionFromFilename(%q) version", c.in)
	}
}

// newTestServerWithUpstream builds a test Backend wired to the given upstream
// fake, following the same allow-private-IPs dance the npm/cargo federation
// tests use for httptest servers reached over loopback.
func newTestServerWithUpstream(t *testing.T, fetcher *upstream.PyPIFetcher) *httptest.Server {
	t.Helper()
	prev := validate.AllowPrivateIPs.Load()
	validate.AllowPrivateIPs.Store(true)
	t.Cleanup(func() { validate.AllowPrivateIPs.Store(prev) })

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
		Upstream: fetcher,
		Logger:   nopLog(),
	})
	srv.Config.Handler = b.Handler()
	return srv
}

func TestSimpleIndexServesUpstreamOnLocalMiss(t *testing.T) {
	var requests []string
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.Path)
		w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
		_, _ = w.Write([]byte(`{"files":[{"filename":"foo_bar-1.0-py3-none-any.whl",` +
			`"url":"https://files.pythonhosted.org/foo_bar-1.0-py3-none-any.whl",` +
			`"hashes":{"sha256":"deadbeef"},` +
			`"requires-python":">=3.9"}]}`))
	}))
	defer upstreamSrv.Close()

	fetcher := upstream.NewPyPIFetcher([]string{upstreamSrv.URL}, time.Second*5, 1<<20)
	srv := newTestServerWithUpstream(t, fetcher)

	resp := doGet(t, srv, "/pypi/simple/foo-bar/")
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/html")
	body, _ := io.ReadAll(resp.Body)
	html := string(body)
	assert.Contains(t, html, "/pypi/files/foo-bar/1.0/foo_bar-1.0-py3-none-any.whl")
	assert.Contains(t, html, "#sha256=deadbeef")
	assert.Contains(t, html, `data-requires-python="&gt;=3.9"`)

	// Denormalized request name must hit upstream at the normalized path.
	resp2 := doGet(t, srv, "/pypi/simple/Foo_Bar/")
	defer func() { _ = resp2.Body.Close() }()
	require.Equal(t, http.StatusOK, resp2.StatusCode)

	for _, p := range requests {
		assert.Equal(t, "/simple/foo-bar/", p)
	}
}

func TestSimpleIndexLocalPackageShadowsUpstream(t *testing.T) {
	upstreamCalls := 0
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
		_, _ = w.Write([]byte(`{"files":[{"filename":"foo-bar-9.9.9.tar.gz","url":"https://x/foo-bar-9.9.9.tar.gz","hashes":{"sha256":"notlocal"},"requires-python":""}]}`))
	}))
	defer upstreamSrv.Close()

	fetcher := upstream.NewPyPIFetcher([]string{upstreamSrv.URL}, time.Second*5, 1<<20)
	srv := newTestServerWithUpstream(t, fetcher)

	resp := uploadRequest(t, srv, uploadOpts{
		name:       "foo-bar",
		version:    testVersion,
		filename:   "foo_bar-1.0.0-py3-none-any.whl",
		content:    []byte("local-wheel"),
		withDigest: true,
	}, true)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()

	resp2 := doGet(t, srv, "/pypi/simple/foo-bar/")
	defer func() { _ = resp2.Body.Close() }()
	require.Equal(t, http.StatusOK, resp2.StatusCode)
	body, _ := io.ReadAll(resp2.Body)
	html := string(body)
	assert.Contains(t, html, "foo_bar-1.0.0-py3-none-any.whl")
	assert.NotContains(t, html, "foo-bar-9.9.9.tar.gz")
	assert.Equal(t, 0, upstreamCalls, "locally-owned package must never contact upstream")
}

func TestSimpleIndex404WhenUpstreamDisabled(t *testing.T) {
	srv := newTestServer(t) // Upstream: nil
	resp := doGet(t, srv, "/pypi/simple/unknown-project/")
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestSimpleIndex404WhenUpstream404s(t *testing.T) {
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstreamSrv.Close()

	fetcher := upstream.NewPyPIFetcher([]string{upstreamSrv.URL}, time.Second*5, 1<<20)
	srv := newTestServerWithUpstream(t, fetcher)

	resp := doGet(t, srv, "/pypi/simple/unknown-project/")
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

package pypi

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
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
// tests use for httptest servers reached over loopback. The DB is returned
// alongside the server so cache-fill tests can assert on package/blob rows
// directly.
func newTestServerWithUpstream(t *testing.T, fetcher *upstream.PyPIFetcher) (*httptest.Server, *database.DB) {
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
	return srv, db
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
	srv, _ := newTestServerWithUpstream(t, fetcher)

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

	require.Len(t, requests, 2, "both the original and denormalized requests must reach upstream")
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
	srv, _ := newTestServerWithUpstream(t, fetcher)

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
	srv, _ := newTestServerWithUpstream(t, fetcher)

	resp := doGet(t, srv, "/pypi/simple/unknown-project/")
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestDownloadCachesFromUpstreamOnMiss(t *testing.T) {
	wheel := []byte("fake wheel bytes for cache-through test")
	sum := sha256.Sum256(wheel)
	wheelDigest := hex.EncodeToString(sum[:])

	var fileRequests int
	var upstreamSrv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/simple/foo-bar/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
		_, _ = fmt.Fprintf(w, `{"files":[{"filename":"foo_bar-1.0-py3-none-any.whl",`+
			`"url":"%s/files/foo_bar-1.0-py3-none-any.whl",`+
			`"hashes":{"sha256":"%s"},"requires-python":">=3.9"}]}`, upstreamSrv.URL, wheelDigest)
	})
	mux.HandleFunc("/files/foo_bar-1.0-py3-none-any.whl", func(w http.ResponseWriter, r *http.Request) {
		fileRequests++
		_, _ = w.Write(wheel)
	})
	upstreamSrv = httptest.NewServer(mux)
	defer upstreamSrv.Close()

	fetcher := upstream.NewPyPIFetcher([]string{upstreamSrv.URL}, time.Second*5, 1<<20)
	srv, db := newTestServerWithUpstream(t, fetcher)

	resp := doGet(t, srv, "/pypi/files/foo-bar/1.0/foo_bar-1.0-py3-none-any.whl")
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, wheel, got)

	pkg, err := db.GetPackage(t.Context(), packageType, "foo-bar")
	require.NoError(t, err)
	require.NotNil(t, pkg, "cache-fill must create a package row")
	assert.Equal(t, upstreamOwner, pkg.OwnerID)

	resp2 := doGet(t, srv, "/pypi/files/foo-bar/1.0/foo_bar-1.0-py3-none-any.whl")
	defer func() { _ = resp2.Body.Close() }()
	require.Equal(t, http.StatusOK, resp2.StatusCode)
	got2, _ := io.ReadAll(resp2.Body)
	assert.Equal(t, wheel, got2)

	assert.Equal(t, 1, fileRequests, "the upstream file endpoint must be hit exactly once; the second serve is local")
}

func TestDownloadSha256MismatchNotStored(t *testing.T) {
	actual := []byte("these are the actual served bytes, which do not match the index")
	badDigest := strings.Repeat("0", 64)

	var upstreamSrv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/simple/bad-hash/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
		_, _ = fmt.Fprintf(w, `{"files":[{"filename":"bad_hash-1.0-py3-none-any.whl",`+
			`"url":"%s/files/bad_hash-1.0-py3-none-any.whl",`+
			`"hashes":{"sha256":"%s"},"requires-python":""}]}`, upstreamSrv.URL, badDigest)
	})
	mux.HandleFunc("/files/bad_hash-1.0-py3-none-any.whl", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(actual)
	})
	upstreamSrv = httptest.NewServer(mux)
	defer upstreamSrv.Close()

	fetcher := upstream.NewPyPIFetcher([]string{upstreamSrv.URL}, time.Second*5, 1<<20)
	srv, db := newTestServerWithUpstream(t, fetcher)

	resp := doGet(t, srv, "/pypi/files/bad-hash/1.0/bad_hash-1.0-py3-none-any.whl")
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)

	pkg, err := db.GetPackage(t.Context(), packageType, "bad-hash")
	require.NoError(t, err)
	assert.Nil(t, pkg, "no package row must be created on a sha256 mismatch")

	sum := sha256.Sum256(actual)
	blob, err := db.GetBlob(t.Context(), "sha256:"+hex.EncodeToString(sum[:]))
	require.NoError(t, err)
	assert.Nil(t, blob, "no blob row must be created on a sha256 mismatch")
	// package_files rows always belong to a version, which always belongs to
	// a package; the absent package row above proves no package_files row
	// referencing this download exists either.
}

func TestDownloadNormalizationSingleCacheEntry(t *testing.T) {
	wheel := []byte("normalization test wheel bytes")
	sum := sha256.Sum256(wheel)
	digest := hex.EncodeToString(sum[:])

	var upstreamSrv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/simple/foo-bar/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
		_, _ = fmt.Fprintf(w, `{"files":[{"filename":"foo_bar-1.0-py3-none-any.whl",`+
			`"url":"%s/files/foo_bar-1.0-py3-none-any.whl",`+
			`"hashes":{"sha256":"%s"},"requires-python":""}]}`, upstreamSrv.URL, digest)
	})
	mux.HandleFunc("/files/foo_bar-1.0-py3-none-any.whl", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(wheel)
	})
	upstreamSrv = httptest.NewServer(mux)
	defer upstreamSrv.Close()

	fetcher := upstream.NewPyPIFetcher([]string{upstreamSrv.URL}, time.Second*5, 1<<20)
	srv, db := newTestServerWithUpstream(t, fetcher)

	resp := doGet(t, srv, "/pypi/files/Foo_Bar/1.0/foo_bar-1.0-py3-none-any.whl")
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	resp2 := doGet(t, srv, "/pypi/files/foo.bar/1.0/foo_bar-1.0-py3-none-any.whl")
	defer func() { _ = resp2.Body.Close() }()
	require.Equal(t, http.StatusOK, resp2.StatusCode)

	pkgs, err := db.ListPackages(t.Context(), packageType, "", 100)
	require.NoError(t, err)
	require.Len(t, pkgs, 1, "denormalized requests must share a single cache entry")
	assert.Equal(t, "foo-bar", pkgs[0].Name)
}

func TestDownloadUpstreamMissingFileStill404(t *testing.T) {
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
		_, _ = w.Write([]byte(`{"files":[{"filename":"other-2.0-py3-none-any.whl",` +
			`"url":"https://x/other-2.0-py3-none-any.whl",` +
			`"hashes":{"sha256":"deadbeef"},"requires-python":""}]}`))
	}))
	defer upstreamSrv.Close()

	fetcher := upstream.NewPyPIFetcher([]string{upstreamSrv.URL}, time.Second*5, 1<<20)
	srv, _ := newTestServerWithUpstream(t, fetcher)

	resp := doGet(t, srv, "/pypi/files/missing-file-pkg/1.0/missing_file_pkg-1.0-py3-none-any.whl")
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestDownloadLocalPackageNeverFetchesUpstream(t *testing.T) {
	upstreamCalls := 0
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstreamSrv.Close()

	fetcher := upstream.NewPyPIFetcher([]string{upstreamSrv.URL}, time.Second*5, 1<<20)
	srv, _ := newTestServerWithUpstream(t, fetcher)

	resp := uploadRequest(t, srv, uploadOpts{
		name:       "local-only",
		version:    testVersion,
		filename:   "local_only-1.0.0-py3-none-any.whl",
		content:    []byte("local bytes"),
		withDigest: true,
	}, true)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()

	resp2 := doGet(t, srv, "/pypi/files/local-only/9.9.9/local_only-9.9.9-py3-none-any.whl")
	defer func() { _ = resp2.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp2.StatusCode)
	assert.Equal(t, 0, upstreamCalls, "a locally-owned package must never trigger an upstream fetch")
}

// TestSimpleIndexFallsBackToCachedListingOnUpstreamError exercises the fallback
// branch in handleSimplePackage, reachable only once a package is owned by
// upstreamOwner — i.e. after a cache-fill has run at least once.
func TestSimpleIndexFallsBackToCachedListingOnUpstreamError(t *testing.T) {
	wheel := []byte("fallback test wheel bytes")
	sum := sha256.Sum256(wheel)
	digest := hex.EncodeToString(sum[:])

	var upstreamSrv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/simple/foo-bar/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
		_, _ = fmt.Fprintf(w, `{"files":[{"filename":"foo_bar-1.0-py3-none-any.whl",`+
			`"url":"%s/files/foo_bar-1.0-py3-none-any.whl",`+
			`"hashes":{"sha256":"%s"},"requires-python":""}]}`, upstreamSrv.URL, digest)
	})
	mux.HandleFunc("/files/foo_bar-1.0-py3-none-any.whl", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(wheel)
	})
	upstreamSrv = httptest.NewServer(mux)

	fetcher := upstream.NewPyPIFetcher([]string{upstreamSrv.URL}, time.Second*5, 1<<20)
	srv, _ := newTestServerWithUpstream(t, fetcher)

	// Seed a cached package via one successful cache-through download.
	resp := doGet(t, srv, "/pypi/files/foo-bar/1.0/foo_bar-1.0-py3-none-any.whl")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()

	// The upstream index is now unreachable.
	upstreamSrv.Close()

	resp2 := doGet(t, srv, "/pypi/simple/foo-bar/")
	defer func() { _ = resp2.Body.Close() }()
	require.Equal(t, http.StatusOK, resp2.StatusCode)
	body, _ := io.ReadAll(resp2.Body)
	assert.Contains(t, string(body), "foo_bar-1.0-py3-none-any.whl", "must fall back to the cached local listing")
}

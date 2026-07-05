package pypi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/blobstore"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
)

const (
	testToken    = "test-token"
	testOwnerURL = "https://alice.example.com/ap/actor"
	testVersion  = "1.0.0"
	testPkgDemo  = "demo"
	testFileTgz  = "demo-1.0.0.tar.gz"
)

func nopLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestServer(t *testing.T) *httptest.Server {
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
		Logger:   nopLog(),
	})
	srv.Config.Handler = b.Handler()
	return srv
}

type uploadOpts struct {
	name           string
	version        string
	filename       string
	content        []byte
	withDigest     bool
	overrideDigest string
	requiresPython string
}

func uploadRequest(t *testing.T, srv *httptest.Server, o uploadOpts, withAuth bool) *http.Response {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	require.NoError(t, w.WriteField("name", o.name))
	require.NoError(t, w.WriteField("version", o.version))
	if o.requiresPython != "" {
		require.NoError(t, w.WriteField("requires_python", o.requiresPython))
	}
	if o.withDigest {
		sum := sha256.Sum256(o.content)
		digest := hex.EncodeToString(sum[:])
		if o.overrideDigest != "" {
			digest = o.overrideDigest
		}
		require.NoError(t, w.WriteField("sha256_digest", digest))
	}
	fw, err := w.CreateFormFile("content", o.filename)
	require.NoError(t, err)
	_, err = fw.Write(o.content)
	require.NoError(t, err)
	require.NoError(t, w.Close())

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/pypi/", &body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", w.FormDataContentType())
	if withAuth {
		req.SetBasicAuth("__token__", testToken)
	}
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	return resp
}

func TestUploadAndDownload(t *testing.T) {
	srv := newTestServer(t)

	wheel := []byte("PK\x03\x04 wheel-bytes")
	resp := uploadRequest(t, srv, uploadOpts{
		name:       "Requests",
		version:    "2.31.0",
		filename:   "requests-2.31.0-py3-none-any.whl",
		content:    wheel,
		withDigest: true,
	}, true)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	resp = doGet(t, srv, "/pypi/files/requests/2.31.0/requests-2.31.0-py3-none-any.whl")
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, wheel, got)
	assert.NotEmpty(t, resp.Header.Get("ETag"))
}

func TestUploadNormalizesName(t *testing.T) {
	srv := newTestServer(t)

	resp := uploadRequest(t, srv, uploadOpts{
		name:     "My_Project.Name",
		version:  testVersion,
		filename: "my_project_name-1.0.0-py3-none-any.whl",
		content:  []byte("data"),
	}, true)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Normalized to "my-project-name"
	resp = doGet(t, srv, "/pypi/simple/My_Project.Name/")
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	doc, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(doc), "my_project_name-1.0.0-py3-none-any.whl")

	resp = doGet(t, srv, "/pypi/simple/my-project-name/")
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestSimpleIndex(t *testing.T) {
	srv := newTestServer(t)

	for i, v := range []string{"1.0.0", "1.0.1"} {
		_ = i
		resp := uploadRequest(t, srv, uploadOpts{
			name:           testPkgDemo,
			version:        v,
			filename:       "demo-" + v + "-py3-none-any.whl",
			content:        []byte("payload-" + v),
			withDigest:     true,
			requiresPython: ">=3.8",
		}, true)
		require.Equal(t, http.StatusOK, resp.StatusCode, "upload %s", v)
		_ = resp.Body.Close()
	}

	resp := doGet(t, srv, "/pypi/simple/demo/")
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	html := string(body)
	assert.Contains(t, html, `data-requires-python=`)
	assert.Contains(t, html, "demo-1.0.0-py3-none-any.whl")
	assert.Contains(t, html, "demo-1.0.1-py3-none-any.whl")
	assert.Contains(t, html, "#sha256=")

	root := doGet(t, srv, "/pypi/simple/")
	defer func() { _ = root.Body.Close() }()
	require.Equal(t, http.StatusOK, root.StatusCode)
	rootBody, _ := io.ReadAll(root.Body)
	assert.Contains(t, string(rootBody), `<a href="demo/">demo</a>`)
}

func TestUploadUnauthorized(t *testing.T) {
	srv := newTestServer(t)
	resp := uploadRequest(t, srv, uploadOpts{
		name: testPkgDemo, version: testVersion,
		filename: testFileTgz, content: []byte("x"),
	}, false)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestUploadBadDigest(t *testing.T) {
	srv := newTestServer(t)
	resp := uploadRequest(t, srv, uploadOpts{
		name: testPkgDemo, version: testVersion,
		filename:       testFileTgz,
		content:        []byte("hello"),
		withDigest:     true,
		overrideDigest: strings.Repeat("0", 64),
	}, true)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestUploadDuplicateFile(t *testing.T) {
	srv := newTestServer(t)
	o := uploadOpts{
		name: testPkgDemo, version: testVersion,
		filename: testFileTgz, content: []byte("payload"),
	}
	resp := uploadRequest(t, srv, o, true)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()

	resp2 := uploadRequest(t, srv, o, true)
	defer func() { _ = resp2.Body.Close() }()
	assert.Equal(t, http.StatusConflict, resp2.StatusCode)
}

func TestUploadMultipleFilesSameVersion(t *testing.T) {
	srv := newTestServer(t)

	resp := uploadRequest(t, srv, uploadOpts{
		name: testPkgDemo, version: testVersion,
		filename: testFileTgz, content: []byte("sdist"),
	}, true)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()

	resp2 := uploadRequest(t, srv, uploadOpts{
		name: testPkgDemo, version: testVersion,
		filename: "demo-1.0.0-py3-none-any.whl", content: []byte("wheel"),
	}, true)
	require.Equal(t, http.StatusOK, resp2.StatusCode)
	_ = resp2.Body.Close()

	resp3 := doGet(t, srv, "/pypi/simple/demo/")
	defer func() { _ = resp3.Body.Close() }()
	body, _ := io.ReadAll(resp3.Body)
	html := string(body)
	assert.Contains(t, html, "demo-1.0.0.tar.gz")
	assert.Contains(t, html, "demo-1.0.0-py3-none-any.whl")
}

func TestSimplePackageNotFound(t *testing.T) {
	srv := newTestServer(t)
	resp := doGet(t, srv, "/pypi/simple/missing/")
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestDownloadNotFound(t *testing.T) {
	srv := newTestServer(t)

	// Unknown project.
	resp := doGet(t, srv, "/pypi/files/missing/1.0.0/missing-1.0.0.tar.gz")
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, "project not found\n", string(body))

	uploadResp := uploadRequest(t, srv, uploadOpts{
		name: testPkgDemo, version: testVersion,
		filename: testFileTgz, content: []byte("data"), withDigest: true,
	}, true)
	defer func() { _ = uploadResp.Body.Close() }()
	require.Equal(t, http.StatusOK, uploadResp.StatusCode)

	// Known project, unknown version.
	resp2 := doGet(t, srv, "/pypi/files/"+testPkgDemo+"/9.9.9/demo-9.9.9.tar.gz")
	defer func() { _ = resp2.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp2.StatusCode)
	body2, _ := io.ReadAll(resp2.Body)
	assert.Equal(t, "version not found\n", string(body2))

	// Known project + version, unknown filename.
	resp3 := doGet(t, srv, "/pypi/files/"+testPkgDemo+"/"+testVersion+"/missing-1.0.0.tar.gz")
	defer func() { _ = resp3.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp3.StatusCode)
	body3, _ := io.ReadAll(resp3.Body)
	assert.Equal(t, "file not found\n", string(body3))
}

func TestNormalizeName(t *testing.T) {
	cases := map[string]string{
		"requests":        "requests",
		"My_Project.Name": "my-project-name",
		"foo--bar":        "foo-bar",
		"FOO_BAR-baz":     "foo-bar-baz",
		"a.b_c-d":         "a-b-c-d",
	}
	for in, want := range cases {
		assert.Equal(t, want, normalizeName(in), "normalizeName(%q)", in)
	}
}

func TestUploadBearerAuth(t *testing.T) {
	srv := newTestServer(t)

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	require.NoError(t, w.WriteField("name", "demo"))
	require.NoError(t, w.WriteField("version", "1.0.0"))
	fw, err := w.CreateFormFile("content", "demo-1.0.0.tar.gz")
	require.NoError(t, err)
	_, err = fw.Write([]byte("data"))
	require.NoError(t, err)
	require.NoError(t, w.Close())

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/pypi/", &body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestUnconfiguredOwnerRejected(t *testing.T) {
	db, err := database.OpenSQLite(t.TempDir(), 0, 0, nopLog())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	blobs, err := blobstore.New(t.TempDir(), nopLog())
	require.NoError(t, err)

	_, err = db.GetOrCreatePackage(t.Context(), packageType, "taken", "https://other.example.com/ap/actor")
	require.NoError(t, err)

	srv := httptest.NewServer(nil)
	t.Cleanup(srv.Close)
	b := New(Config{DB: db, Blobs: blobs, Endpoint: srv.URL, Token: testToken, Owner: testOwnerURL, Logger: nopLog()})
	srv.Config.Handler = b.Handler()

	resp := uploadRequest(t, srv, uploadOpts{
		name: "taken", version: testVersion,
		filename: "taken-1.0.0.tar.gz", content: []byte("x"),
	}, true)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func doGet(t *testing.T, srv *httptest.Server, path string) *http.Response {
	t.Helper()
	resp, err := srv.Client().Get(srv.URL + path)
	require.NoError(t, err)
	return resp
}

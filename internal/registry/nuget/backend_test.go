package nuget

import (
	"archive/zip"
	"bytes"
	"encoding/json"
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
	testVersion2 = "2.0.0"
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

func buildNupkg(id, version string) []byte {
	nuspecContent := `<?xml version="1.0" encoding="utf-8"?>
<package xmlns="http://schemas.microsoft.com/packaging/2013/05/nuspec.xsd">
  <metadata>
    <id>` + id + `</id>
    <version>` + version + `</version>
    <authors>Test Author</authors>
    <description>A test package</description>
  </metadata>
</package>`

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, _ := zw.Create(strings.ToLower(id) + ".nuspec")
	_, _ = f.Write([]byte(nuspecContent))
	_ = zw.Close()
	return buf.Bytes()
}

func pushPackage(t *testing.T, srv *httptest.Server, id, version string, withAuth bool) *http.Response {
	t.Helper()
	nupkg := buildNupkg(id, version)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("package", strings.ToLower(id)+"."+strings.ToLower(version)+".nupkg")
	require.NoError(t, err)
	_, err = fw.Write(nupkg)
	require.NoError(t, err)
	require.NoError(t, mw.Close())

	req, err := http.NewRequest(http.MethodPut, srv.URL+"/nuget/v3/package", &body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if withAuth {
		req.Header.Set("X-NuGet-ApiKey", testToken)
	}
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	return resp
}

func doGet(t *testing.T, srv *httptest.Server, path string) *http.Response {
	t.Helper()
	resp, err := srv.Client().Get(srv.URL + path)
	require.NoError(t, err)
	return resp
}

func TestServiceIndex(t *testing.T) {
	srv := newTestServer(t)

	resp := doGet(t, srv, "/nuget/v3/index.json")
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var idx serviceIndex
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&idx))
	assert.Equal(t, "3.0.0", idx.Version)
	require.Len(t, idx.Resources, 3)

	types := make(map[string]string)
	for _, r := range idx.Resources {
		types[r.Type] = r.ID
	}
	assert.Contains(t, types["PackageBaseAddress/3.0.0"], "/v3-flatcontainer/")
	assert.Contains(t, types["PackagePublish/2.0.0"], "/v3/package")
	assert.Contains(t, types["RegistrationsBaseUrl/3.0.0"], "/v3/registration/")
}

func TestPushAndDownload(t *testing.T) {
	srv := newTestServer(t)
	nupkg := buildNupkg("MyPackage", testVersion)

	resp := pushPackage(t, srv, "MyPackage", testVersion, true)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	resp = doGet(t, srv, "/nuget/v3-flatcontainer/mypackage/1.0.0/mypackage.1.0.0.nupkg")
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, nupkg, got)
}

// Official NuGet clients (dotnet/nuget.exe) append a trailing slash to the
// PackagePublish resource @id, so pushes land on "/v3/package/" rather than
// "/v3/package". Both must be accepted.
func TestPushTrailingSlash(t *testing.T) {
	srv := newTestServer(t)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("package", "mypkg.1.0.0.nupkg")
	require.NoError(t, err)
	_, err = fw.Write(buildNupkg("mypkg", testVersion))
	require.NoError(t, err)
	require.NoError(t, mw.Close())

	req, err := http.NewRequest(http.MethodPut, srv.URL+"/nuget/v3/package/", &body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-NuGet-ApiKey", testToken)
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
}

func TestPushNormalizesID(t *testing.T) {
	srv := newTestServer(t)

	resp := pushPackage(t, srv, "MyPackage", testVersion, true)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	_ = resp.Body.Close()

	// Download using lowercase ID
	resp = doGet(t, srv, "/nuget/v3-flatcontainer/mypackage/1.0.0/mypackage.1.0.0.nupkg")
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestVersionList(t *testing.T) {
	srv := newTestServer(t)

	for _, v := range []string{testVersion, testVersion2} {
		resp := pushPackage(t, srv, "mypkg", v, true)
		require.Equal(t, http.StatusCreated, resp.StatusCode, "push %s", v)
		_ = resp.Body.Close()
	}

	resp := doGet(t, srv, "/nuget/v3-flatcontainer/mypkg/index.json")
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string][]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	assert.ElementsMatch(t, []string{testVersion, testVersion2}, result["versions"])
}

func TestRegistrationIndex(t *testing.T) {
	srv := newTestServer(t)

	resp := pushPackage(t, srv, "mypkg", "1.2.3", true)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	_ = resp.Body.Close()

	resp = doGet(t, srv, "/nuget/v3/registration/mypkg/index.json")
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var idx registrationIndex
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&idx))
	require.Len(t, idx.Items, 1)
	require.Len(t, idx.Items[0].Items, 1)
	leaf := idx.Items[0].Items[0]
	assert.Equal(t, "1.2.3", leaf.CatalogEntry.Version)
	assert.Equal(t, "Test Author", leaf.CatalogEntry.Authors)
	assert.Contains(t, leaf.PackageContent, "/v3-flatcontainer/")
}

func TestRegistrationLeaf(t *testing.T) {
	srv := newTestServer(t)

	resp := pushPackage(t, srv, "mypkg", testVersion, true)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	_ = resp.Body.Close()

	resp = doGet(t, srv, "/nuget/v3/registration/mypkg/1.0.0.json")
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var leaf registrationLeaf
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&leaf))
	assert.Equal(t, testVersion, leaf.CatalogEntry.Version)
	assert.Equal(t, "A test package", leaf.CatalogEntry.Description)
}

func TestPushConflict(t *testing.T) {
	srv := newTestServer(t)

	resp := pushPackage(t, srv, "mypkg", testVersion, true)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	_ = resp.Body.Close()

	resp = pushPackage(t, srv, "mypkg", testVersion, true)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
}

func TestPushUnauthorized(t *testing.T) {
	srv := newTestServer(t)

	resp := pushPackage(t, srv, "mypkg", testVersion, false)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestPushInvalidNupkg(t *testing.T) {
	srv := newTestServer(t)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("package", "bad.nupkg")
	require.NoError(t, err)
	_, err = fw.Write([]byte("not a zip file"))
	require.NoError(t, err)
	require.NoError(t, mw.Close())

	req, err := http.NewRequest(http.MethodPut, srv.URL+"/nuget/v3/package", &body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-NuGet-ApiKey", testToken)
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestDeleteVersion(t *testing.T) {
	srv := newTestServer(t)

	resp := pushPackage(t, srv, "mypkg", testVersion, true)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	_ = resp.Body.Close()

	req, err := http.NewRequest(http.MethodDelete, srv.URL+"/nuget/v3/package/mypkg/1.0.0", nil)
	require.NoError(t, err)
	req.Header.Set("X-NuGet-ApiKey", testToken)
	resp, err = srv.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)

	resp = doGet(t, srv, "/nuget/v3-flatcontainer/mypkg/index.json")
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var result map[string][]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	assert.Empty(t, result["versions"])
}

func TestDeleteUnauthorized(t *testing.T) {
	srv := newTestServer(t)

	resp := pushPackage(t, srv, "mypkg", testVersion, true)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	_ = resp.Body.Close()

	req, err := http.NewRequest(http.MethodDelete, srv.URL+"/nuget/v3/package/mypkg/1.0.0", nil)
	require.NoError(t, err)
	resp, err = srv.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestDownloadNotFound(t *testing.T) {
	srv := newTestServer(t)
	resp := doGet(t, srv, "/nuget/v3-flatcontainer/missing/1.0.0/missing.1.0.0.nupkg")
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestVersionListNotFound(t *testing.T) {
	srv := newTestServer(t)
	resp := doGet(t, srv, "/nuget/v3-flatcontainer/missing/index.json")
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestRegistrationNotFound(t *testing.T) {
	srv := newTestServer(t)
	resp := doGet(t, srv, "/nuget/v3/registration/missing/index.json")
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// --- auth edge cases ---

func TestUnauthorizedHasWWWAuthenticate(t *testing.T) {
	srv := newTestServer(t)
	resp := pushPackage(t, srv, "mypkg", testVersion, false)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Equal(t, `ApiKey realm="apoci"`, resp.Header.Get("WWW-Authenticate"))
}

func TestNoTokenRequired(t *testing.T) {
	db, err := database.OpenSQLite(t.TempDir(), 0, 0, nopLog())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	blobs, err := blobstore.New(t.TempDir(), nopLog())
	require.NoError(t, err)

	srv := httptest.NewServer(nil)
	t.Cleanup(srv.Close)
	b := New(Config{DB: db, Blobs: blobs, Endpoint: srv.URL, Token: "", Owner: testOwnerURL, Logger: nopLog()})
	srv.Config.Handler = b.Handler()

	resp := pushPackage(t, srv, "mypkg", testVersion, false)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
}

// --- push validation ---

func TestPushNoPackageField(t *testing.T) {
	srv := newTestServer(t)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("other", "mypkg.1.0.0.nupkg")
	require.NoError(t, err)
	_, err = fw.Write(buildNupkg("mypkg", testVersion))
	require.NoError(t, err)
	require.NoError(t, mw.Close())

	req, err := http.NewRequest(http.MethodPut, srv.URL+"/nuget/v3/package", &body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-NuGet-ApiKey", testToken)
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestPushNoNuspecInZip(t *testing.T) {
	srv := newTestServer(t)

	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	f, _ := zw.Create("content.txt")
	_, _ = f.Write([]byte("no nuspec here"))
	_ = zw.Close()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("package", "mypkg.1.0.0.nupkg")
	require.NoError(t, err)
	_, err = fw.Write(zipBuf.Bytes())
	require.NoError(t, err)
	require.NoError(t, mw.Close())

	req, err := http.NewRequest(http.MethodPut, srv.URL+"/nuget/v3/package", &body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-NuGet-ApiKey", testToken)
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestPushMalformedNuspecXML(t *testing.T) {
	srv := newTestServer(t)

	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	f, _ := zw.Create("mypkg.nuspec")
	_, _ = f.Write([]byte("<<< not xml >>>"))
	_ = zw.Close()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("package", "mypkg.1.0.0.nupkg")
	require.NoError(t, err)
	_, err = fw.Write(zipBuf.Bytes())
	require.NoError(t, err)
	require.NoError(t, mw.Close())

	req, err := http.NewRequest(http.MethodPut, srv.URL+"/nuget/v3/package", &body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-NuGet-ApiKey", testToken)
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func buildNupkgWithMeta(id, version string) []byte {
	nuspecContent := `<?xml version="1.0" encoding="utf-8"?>
<package xmlns="http://schemas.microsoft.com/packaging/2013/05/nuspec.xsd">
  <metadata>
    <id>` + id + `</id>
    <version>` + version + `</version>
  </metadata>
</package>`
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, _ := zw.Create("pkg.nuspec")
	_, _ = f.Write([]byte(nuspecContent))
	_ = zw.Close()
	return buf.Bytes()
}

func pushRaw(t *testing.T, srv *httptest.Server, nupkg []byte) *http.Response {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("package", "pkg.nupkg")
	require.NoError(t, err)
	_, err = fw.Write(nupkg)
	require.NoError(t, err)
	require.NoError(t, mw.Close())
	req, err := http.NewRequest(http.MethodPut, srv.URL+"/nuget/v3/package", &body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-NuGet-ApiKey", testToken)
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	return resp
}

func TestPushNuspecMissingID(t *testing.T) {
	srv := newTestServer(t)
	resp := pushRaw(t, srv, buildNupkgWithMeta("", testVersion))
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestPushNuspecMissingVersion(t *testing.T) {
	srv := newTestServer(t)
	resp := pushRaw(t, srv, buildNupkgWithMeta("mypkg", ""))
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// --- download edge cases ---

func TestDownloadVersionNotFound(t *testing.T) {
	srv := newTestServer(t)

	resp := pushPackage(t, srv, "mypkg", testVersion, true)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	_ = resp.Body.Close()

	resp = doGet(t, srv, "/nuget/v3-flatcontainer/mypkg/9.9.9/mypkg.9.9.9.nupkg")
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestDownloadWrongFilename(t *testing.T) {
	srv := newTestServer(t)

	resp := pushPackage(t, srv, "mypkg", testVersion, true)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	_ = resp.Body.Close()

	resp = doGet(t, srv, "/nuget/v3-flatcontainer/mypkg/1.0.0/wrong.1.0.0.nupkg")
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// --- registration leaf edge cases ---

func TestRegistrationLeafNoJsonSuffix(t *testing.T) {
	srv := newTestServer(t)

	resp := pushPackage(t, srv, "mypkg", testVersion, true)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	_ = resp.Body.Close()

	// Missing .json extension — should not match
	resp = doGet(t, srv, "/nuget/v3/registration/mypkg/1.0.0")
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestRegistrationLeafVersionNotFound(t *testing.T) {
	srv := newTestServer(t)

	resp := pushPackage(t, srv, "mypkg", testVersion, true)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	_ = resp.Body.Close()

	resp = doGet(t, srv, "/nuget/v3/registration/mypkg/9.9.9.json")
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestRegistrationIndexLowerUpper(t *testing.T) {
	srv := newTestServer(t)

	for _, v := range []string{testVersion, testVersion2, "3.0.0"} {
		resp := pushPackage(t, srv, "mypkg", v, true)
		require.Equal(t, http.StatusCreated, resp.StatusCode)
		_ = resp.Body.Close()
	}

	resp := doGet(t, srv, "/nuget/v3/registration/mypkg/index.json")
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var idx registrationIndex
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&idx))
	require.Len(t, idx.Items, 1)
	assert.Equal(t, testVersion, idx.Items[0].Lower)
	assert.Equal(t, "3.0.0", idx.Items[0].Upper)
}

// --- delete edge cases ---

func TestDeleteNonExistentPackage(t *testing.T) {
	srv := newTestServer(t)

	req, err := http.NewRequest(http.MethodDelete, srv.URL+"/nuget/v3/package/missing/1.0.0", nil)
	require.NoError(t, err)
	req.Header.Set("X-NuGet-ApiKey", testToken)
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestDeleteNonExistentVersionIsIdempotent(t *testing.T) {
	srv := newTestServer(t)

	resp := pushPackage(t, srv, "mypkg", testVersion, true)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	_ = resp.Body.Close()

	req, err := http.NewRequest(http.MethodDelete, srv.URL+"/nuget/v3/package/mypkg/9.9.9", nil)
	require.NoError(t, err)
	req.Header.Set("X-NuGet-ApiKey", testToken)
	resp, err = srv.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
}

// --- metadata preservation ---

func TestCatalogEntryPreservesOriginalID(t *testing.T) {
	srv := newTestServer(t)

	resp := pushPackage(t, srv, "MyPackage", testVersion, true)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	_ = resp.Body.Close()

	resp = doGet(t, srv, "/nuget/v3/registration/mypackage/1.0.0.json")
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var leaf registrationLeaf
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&leaf))
	assert.Equal(t, "MyPackage", leaf.CatalogEntry.PackageID)
}

func TestOwnerMismatch(t *testing.T) {
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

	resp := pushPackage(t, srv, "taken", testVersion, true)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

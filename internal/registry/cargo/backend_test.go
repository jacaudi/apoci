package cargo

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/blobstore"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
)

const testToken = "test-token"

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
		Owner:    "https://alice.example.com/ap/actor",
		Logger:   nopLog(),
	})
	srv.Config.Handler = b.Handler()
	return srv
}

// Wire format: u32 LE meta-len, meta JSON, u32 LE crate-len, crate bytes.
func publishBody(t *testing.T, name, version string, deps []map[string]any, crate []byte) []byte {
	t.Helper()
	if deps == nil {
		deps = []map[string]any{}
	}
	meta := map[string]any{
		"name":        name,
		"vers":        version,
		"description": "test crate",
		"authors":     []string{"alice"},
		"deps":        deps,
		"features":    map[string][]string{},
		"license":     "MIT",
		"homepage":    "https://example.com",
	}
	metaJSON, err := json.Marshal(meta)
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, binary.Write(&buf, binary.LittleEndian, uint32(len(metaJSON)))) //nolint:gosec // test fixture, bounded
	buf.Write(metaJSON)
	require.NoError(t, binary.Write(&buf, binary.LittleEndian, uint32(len(crate)))) //nolint:gosec // test fixture, bounded
	buf.Write(crate)
	return buf.Bytes()
}

func doRequest(t *testing.T, srv *httptest.Server, method, path string, body []byte, withAuth bool) *http.Response {
	t.Helper()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, r)
	require.NoError(t, err)
	if withAuth {
		req.Header.Set("Authorization", testToken)
	}
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	return resp
}

func TestPublishAndDownload(t *testing.T) {
	srv := newTestServer(t)

	crate := []byte("fake-crate-bytes")
	body := publishBody(t, "serde", "1.0.0", nil, crate)

	resp := doRequest(t, srv, http.MethodPut, "/cargo/api/v1/crates/new", body, true)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	resp = doRequest(t, srv, http.MethodGet, "/cargo/api/v1/crates/serde/1.0.0/download", nil, false)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, crate, got)
	assert.NotEmpty(t, resp.Header.Get("ETag"))
}

func TestPublishUnauthorized(t *testing.T) {
	srv := newTestServer(t)
	body := publishBody(t, "serde", "1.0.0", nil, []byte("x"))
	resp := doRequest(t, srv, http.MethodPut, "/cargo/api/v1/crates/new", body, false)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestPublishDuplicate(t *testing.T) {
	srv := newTestServer(t)
	body := publishBody(t, "serde", "1.0.0", nil, []byte("crate-bytes"))

	resp := doRequest(t, srv, http.MethodPut, "/cargo/api/v1/crates/new", body, true)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()

	resp2 := doRequest(t, srv, http.MethodPut, "/cargo/api/v1/crates/new", body, true)
	defer func() { _ = resp2.Body.Close() }()
	assert.Equal(t, http.StatusConflict, resp2.StatusCode)
}

func TestIndexEntries(t *testing.T) {
	srv := newTestServer(t)

	deps := []map[string]any{
		{
			"name":             "rand",
			"version_req":      "^0.6",
			"features":         []string{"std"},
			"optional":         false,
			"default_features": true,
			"target":           nil,
			"kind":             "normal",
		},
	}
	for _, v := range []string{"1.0.0", "1.1.0", "2.0.0"} {
		body := publishBody(t, "serde", v, deps, []byte("crate-"+v))
		resp := doRequest(t, srv, http.MethodPut, "/cargo/api/v1/crates/new", body, true)
		require.Equal(t, http.StatusOK, resp.StatusCode, "publish %s", v)
		_ = resp.Body.Close()
	}

	resp := doRequest(t, srv, http.MethodGet, "/cargo/se/rd/serde", nil, false)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "text/plain; charset=utf-8", resp.Header.Get("Content-Type"))

	scanner := bufio.NewScanner(resp.Body)
	var versions []string
	for scanner.Scan() {
		var entry indexEntry
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &entry))
		assert.Equal(t, "serde", entry.Name)
		assert.NotEmpty(t, entry.Cksum)
		assert.Equal(t, 2, entry.V)
		require.Len(t, entry.Deps, 1)
		assert.Equal(t, "rand", entry.Deps[0].Name)
		assert.Equal(t, "^0.6", entry.Deps[0].Req)
		versions = append(versions, entry.Vers)
	}
	require.NoError(t, scanner.Err())
	assert.ElementsMatch(t, []string{"1.0.0", "1.1.0", "2.0.0"}, versions)
}

func TestIndexNotFound(t *testing.T) {
	srv := newTestServer(t)
	resp := doRequest(t, srv, http.MethodGet, "/cargo/3/m/missing", nil, false)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestYankUnyank(t *testing.T) {
	srv := newTestServer(t)

	body := publishBody(t, "serde", "1.0.0", nil, []byte("crate-bytes"))
	resp := doRequest(t, srv, http.MethodPut, "/cargo/api/v1/crates/new", body, true)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()

	resp = doRequest(t, srv, http.MethodDelete, "/cargo/api/v1/crates/serde/1.0.0/yank", nil, true)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	resp2 := doRequest(t, srv, http.MethodGet, "/cargo/se/rd/serde", nil, false)
	defer func() { _ = resp2.Body.Close() }()
	require.Equal(t, http.StatusOK, resp2.StatusCode)
	scanner := bufio.NewScanner(resp2.Body)
	require.True(t, scanner.Scan())
	var entry indexEntry
	require.NoError(t, json.Unmarshal(scanner.Bytes(), &entry))
	assert.True(t, entry.Yanked)

	resp3 := doRequest(t, srv, http.MethodPut, "/cargo/api/v1/crates/serde/1.0.0/unyank", nil, true)
	defer func() { _ = resp3.Body.Close() }()
	require.Equal(t, http.StatusOK, resp3.StatusCode)

	resp4 := doRequest(t, srv, http.MethodGet, "/cargo/se/rd/serde", nil, false)
	defer func() { _ = resp4.Body.Close() }()
	scanner = bufio.NewScanner(resp4.Body)
	require.True(t, scanner.Scan())
	entry = indexEntry{}
	require.NoError(t, json.Unmarshal(scanner.Bytes(), &entry))
	assert.False(t, entry.Yanked)
}

func TestYankNotFound(t *testing.T) {
	srv := newTestServer(t)
	resp := doRequest(t, srv, http.MethodDelete, "/cargo/api/v1/crates/missing/1.0.0/yank", nil, true)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestDownloadNotFound(t *testing.T) {
	srv := newTestServer(t)
	resp := doRequest(t, srv, http.MethodGet, "/cargo/api/v1/crates/missing/1.0.0/download", nil, false)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestConfigJSON(t *testing.T) {
	srv := newTestServer(t)

	resp := doRequest(t, srv, http.MethodGet, "/cargo/config.json", nil, false)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var cfg struct {
		DL           string `json:"dl"`
		API          string `json:"api"`
		AuthRequired bool   `json:"auth-required"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&cfg))
	assert.Equal(t, srv.URL+"/cargo/api/v1/crates", cfg.DL)
	assert.Equal(t, srv.URL+"/cargo", cfg.API)
	assert.True(t, cfg.AuthRequired)
}

func TestPublishInvalidName(t *testing.T) {
	srv := newTestServer(t)
	body := publishBody(t, "0bad", "1.0.0", nil, []byte("x"))
	resp := doRequest(t, srv, http.MethodPut, "/cargo/api/v1/crates/new", body, true)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	bodyBytes, _ := io.ReadAll(resp.Body)
	assert.True(t,
		strings.Contains(string(bodyBytes), "name") || strings.Contains(string(bodyBytes), "invalid"),
		"got %q", string(bodyBytes),
	)
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
	b := New(Config{DB: db, Blobs: blobs, Endpoint: srv.URL, Token: testToken, Owner: "https://alice.example.com/ap/actor", Logger: nopLog()})
	srv.Config.Handler = b.Handler()

	body := publishBody(t, "taken", "1.0.0", nil, []byte("data"))
	resp := doRequest(t, srv, http.MethodPut, "/cargo/api/v1/crates/new", body, true)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

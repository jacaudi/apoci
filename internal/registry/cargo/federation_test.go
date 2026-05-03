package cargo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/blobstore"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
)

type capturedActivity struct {
	Type   string
	Object any
}

type stubPublisher struct {
	mu  sync.Mutex
	out []capturedActivity
}

func (s *stubPublisher) Publish(_ context.Context, activityType string, object any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.out = append(s.out, capturedActivity{Type: activityType, Object: object})
	return nil
}

func (s *stubPublisher) Activities() []capturedActivity {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]capturedActivity{}, s.out...)
}

func newTestServerWithPublisher(t *testing.T, p *stubPublisher) *httptest.Server {
	t.Helper()
	db, err := database.OpenSQLite(t.TempDir(), 0, 0, nopLog())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	blobs, err := blobstore.New(t.TempDir(), nopLog())
	require.NoError(t, err)

	srv := httptest.NewServer(nil)
	t.Cleanup(srv.Close)

	b := New(Config{
		DB:        db,
		Blobs:     blobs,
		Endpoint:  srv.URL,
		Token:     testToken,
		Owner:     "https://alice.example.com/ap/actor",
		Publisher: p,
		Logger:    nopLog(),
	})
	srv.Config.Handler = b.Handler()
	return srv
}

func TestPublishEmitsCreateActivity(t *testing.T) {
	pub := &stubPublisher{}
	srv := newTestServerWithPublisher(t, pub)

	body := publishBody(t, "serde", "1.0.0", nil, []byte("crate"))
	resp := doRequest(t, srv, http.MethodPut, "/cargo/api/v1/crates/new", body, true)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()

	acts := pub.Activities()
	require.Len(t, acts, 1)
	assert.Equal(t, "Create", acts[0].Type)
	v, ok := acts[0].Object.(CargoVersion)
	require.True(t, ok)
	assert.Equal(t, "serde", v.CargoName)
	assert.Equal(t, "1.0.0", v.CargoVersion)
	assert.NotEmpty(t, v.CargoCksum)
	assert.NotEmpty(t, v.CargoBlobSHA)
}

func TestYankAndUnyankEmitActivities(t *testing.T) {
	pub := &stubPublisher{}
	srv := newTestServerWithPublisher(t, pub)

	body := publishBody(t, "serde", "1.0.0", nil, []byte("crate"))
	resp := doRequest(t, srv, http.MethodPut, "/cargo/api/v1/crates/new", body, true)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()

	startCount := len(pub.Activities())

	resp = doRequest(t, srv, http.MethodDelete, "/cargo/api/v1/crates/serde/1.0.0/yank", nil, true)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()

	resp = doRequest(t, srv, http.MethodPut, "/cargo/api/v1/crates/serde/1.0.0/unyank", nil, true)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()

	tail := pub.Activities()[startCount:]
	require.Len(t, tail, 2)
	for _, a := range tail {
		assert.Equal(t, "Update", a.Type)
		_, ok := a.Object.(CargoYank)
		assert.True(t, ok)
	}
	yank0 := tail[0].Object.(CargoYank)
	yank1 := tail[1].Object.(CargoYank)
	assert.True(t, yank0.CargoYanked)
	assert.False(t, yank1.CargoYanked)
}

func TestAdapterIngestRoundTrip(t *testing.T) {
	pub := &stubPublisher{}
	srv := newTestServerWithPublisher(t, pub)
	originActor := srv.URL + "/ap/actor"

	body := publishBody(t, "serde", "1.0.0", nil, []byte("crate"))
	resp := doRequest(t, srv, http.MethodPut, "/cargo/api/v1/crates/new", body, true)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()

	resp = doRequest(t, srv, http.MethodDelete, "/cargo/api/v1/crates/serde/1.0.0/yank", nil, true)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()

	originActs := pub.Activities()
	require.Len(t, originActs, 2)

	peerDB, err := database.OpenSQLite(t.TempDir(), 0, 0, nopLog())
	require.NoError(t, err)
	t.Cleanup(func() { _ = peerDB.Close() })
	peerBlobs, err := blobstore.New(t.TempDir(), nopLog())
	require.NoError(t, err)
	peer := New(Config{
		DB:       peerDB,
		Blobs:    peerBlobs,
		Endpoint: "https://peer.example.com",
		Owner:    "https://peer.example.com/ap/actor",
		Logger:   nopLog(),
	})
	adapter := peer.FederationAdapter()

	for _, act := range originActs {
		raw, err := json.Marshal(act.Object)
		require.NoError(t, err)
		var m map[string]any
		require.NoError(t, json.Unmarshal(raw, &m))
		apType, _ := m["type"].(string)
		require.NoError(t, adapter.Ingest(t.Context(), act.Type, apType, m, originActor))
	}

	pkg, err := peerDB.GetPackage(t.Context(), packageType, "serde")
	require.NoError(t, err)
	require.NotNil(t, pkg)
	v, err := peerDB.GetPackageVersion(t.Context(), pkg.ID, "1.0.0")
	require.NoError(t, err)
	require.NotNil(t, v)
	var stored storedVersion
	require.NoError(t, json.Unmarshal(v.Metadata, &stored))
	assert.True(t, stored.Yanked, "yank should have been replayed")
}

package npm

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

func TestPublishEmitsActivities(t *testing.T) {
	pub := &stubPublisher{}
	srv := newTestServerWithPublisher(t, pub)

	body := publishBody(t, "lodash", "1.0.0", []byte("tarball"), "latest")
	resp := doRequest(t, srv, http.MethodPut, "/npm/lodash", body, true)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	_ = resp.Body.Close()

	acts := pub.Activities()
	require.Len(t, acts, 2)
	assert.Equal(t, "Create", acts[0].Type)
	v, ok := acts[0].Object.(NpmVersion)
	require.True(t, ok)
	assert.Equal(t, "lodash", v.NpmName)
	assert.Equal(t, "1.0.0", v.NpmVersion)
	assert.Contains(t, v.NpmTarball, "/npm/lodash/-/lodash-1.0.0.tgz")
	assert.NotEmpty(t, v.NpmIntegrity)

	assert.Equal(t, "Update", acts[1].Type)
	tag, ok := acts[1].Object.(NpmTag)
	require.True(t, ok)
	assert.Equal(t, "lodash", tag.NpmName)
	assert.Equal(t, "latest", tag.NpmTag)
	assert.Equal(t, "1.0.0", tag.NpmVersion)
}

func TestDistTagPutAndDeleteEmitActivities(t *testing.T) {
	pub := &stubPublisher{}
	srv := newTestServerWithPublisher(t, pub)

	body := publishBody(t, "react", "1.0.0", []byte("data"), "")
	resp := doRequest(t, srv, http.MethodPut, "/npm/react", body, true)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	_ = resp.Body.Close()

	startCount := len(pub.Activities())

	versionJSON, _ := json.Marshal("1.0.0")
	resp = doRequest(t, srv, http.MethodPut, "/npm/-/package/react/dist-tags/latest", versionJSON, true)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()

	resp = doRequest(t, srv, http.MethodDelete, "/npm/-/package/react/dist-tags/latest", nil, true)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()

	tail := pub.Activities()[startCount:]
	require.Len(t, tail, 2)
	assert.Equal(t, "Update", tail[0].Type)
	assert.Equal(t, "Delete", tail[1].Type)
}

func TestAdapterIngestRoundTrip(t *testing.T) {
	pub := &stubPublisher{}
	srv := newTestServerWithPublisher(t, pub)
	originActor := srv.URL + "/ap/actor"

	body := publishBody(t, "lodash", "1.0.0", []byte("tarball"), "latest")
	resp := doRequest(t, srv, http.MethodPut, "/npm/lodash", body, true)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	_ = resp.Body.Close()

	originActs := pub.Activities()
	require.NotEmpty(t, originActs)

	peerDB, err := database.OpenSQLite(t.TempDir(), 0, 0, nopLog())
	require.NoError(t, err)
	t.Cleanup(func() { _ = peerDB.Close() })
	peerBlobs, err := blobstore.New(t.TempDir(), nopLog())
	require.NoError(t, err)
	peer := New(Config{
		DB:       peerDB,
		Blobs:    peerBlobs,
		Endpoint: "https://peer.example.com",
		Token:    "peer-token",
		Owner:    "https://peer.example.com/ap/actor",
		Logger:   nopLog(),
	})
	adapter := peer.FederationAdapter()

	for _, act := range originActs {
		raw, err := json.Marshal(act.Object)
		require.NoError(t, err)
		var asMap map[string]any
		require.NoError(t, json.Unmarshal(raw, &asMap))
		apType, _ := asMap["type"].(string)
		require.NoError(t, adapter.Ingest(t.Context(), act.Type, apType, asMap, originActor))
	}

	pkg, err := peerDB.GetPackage(t.Context(), packageType, "lodash")
	require.NoError(t, err)
	require.NotNil(t, pkg)
	versions, err := peerDB.ListPackageVersions(t.Context(), pkg.ID)
	require.NoError(t, err)
	require.Len(t, versions, 1)
	assert.Equal(t, "1.0.0", versions[0].Version)

	tags, err := peerDB.ListPackageTags(t.Context(), pkg.ID)
	require.NoError(t, err)
	require.Len(t, tags, 1)
	assert.Equal(t, "latest", tags[0].Name)
	assert.Equal(t, "1.0.0", tags[0].Version)
}

func TestAdapterRejectsForeignSender(t *testing.T) {
	db, err := database.OpenSQLite(t.TempDir(), 0, 0, nopLog())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	blobs, err := blobstore.New(t.TempDir(), nopLog())
	require.NoError(t, err)

	_, err = db.GetOrCreatePackage(t.Context(), packageType, "owned", "https://alice.example.com/ap/actor")
	require.NoError(t, err)

	b := New(Config{DB: db, Blobs: blobs, Endpoint: "https://test", Owner: "https://test/ap/actor", Logger: nopLog()})
	adapter := b.FederationAdapter()

	err = adapter.Ingest(t.Context(), "Create", "NpmVersion", map[string]any{
		"npmName":    "owned",
		"npmVersion": "1.0.0",
	}, "https://eve.example.com/ap/actor")
	require.Error(t, err)
	assert.ErrorIs(t, err, database.ErrPackageOwnerMismatch)
}

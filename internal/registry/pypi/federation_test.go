package pypi

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

func TestUploadEmitsCreateActivity(t *testing.T) {
	pub := &stubPublisher{}
	srv := newTestServerWithPublisher(t, pub)

	resp := uploadRequest(t, srv, uploadOpts{
		name: "demo", version: "1.0.0",
		filename: "demo-1.0.0.tar.gz", content: []byte("payload"),
	}, true)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()

	acts := pub.Activities()
	require.Len(t, acts, 1)
	assert.Equal(t, "Create", acts[0].Type)
	f, ok := acts[0].Object.(PypiFile)
	require.True(t, ok)
	assert.Equal(t, "demo", f.PypiName)
	assert.Equal(t, "1.0.0", f.PypiVersion)
	assert.Equal(t, "demo-1.0.0.tar.gz", f.PypiFilename)
	assert.NotEmpty(t, f.PypiBlobSHA)
}

func TestAdapterIngestRoundTrip(t *testing.T) {
	pub := &stubPublisher{}
	srv := newTestServerWithPublisher(t, pub)
	originActor := srv.URL + "/ap/actor"

	for _, fname := range []string{"demo-1.0.0.tar.gz", "demo-1.0.0-py3-none-any.whl"} {
		resp := uploadRequest(t, srv, uploadOpts{
			name: "demo", version: "1.0.0",
			filename: fname, content: []byte("payload-" + fname),
		}, true)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		_ = resp.Body.Close()
	}

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

	pkg, err := peerDB.GetPackage(t.Context(), packageType, "demo")
	require.NoError(t, err)
	require.NotNil(t, pkg)
	v, err := peerDB.GetPackageVersion(t.Context(), pkg.ID, "1.0.0")
	require.NoError(t, err)
	require.NotNil(t, v)
	files, err := peerDB.ListPackageFiles(t.Context(), v.ID)
	require.NoError(t, err)
	assert.Len(t, files, 2, "both sdist and wheel should be replayed")
}

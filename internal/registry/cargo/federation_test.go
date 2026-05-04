package cargo

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/blobstore"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/peering"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/registry/pkgfed/pkgfedtest"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/validate"
)

func newTestServerWithPublisher(t *testing.T, p *pkgfedtest.StubPublisher) *httptest.Server {
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
		Owner:     testOwnerURL,
		Publisher: p,
		Logger:    nopLog(),
	})
	srv.Config.Handler = b.Handler()
	return srv
}

func TestPublishEmitsCreateActivity(t *testing.T) {
	pub := &pkgfedtest.StubPublisher{}
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
	pub := &pkgfedtest.StubPublisher{}
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

func TestEagerReplicationStoresBytesLocally(t *testing.T) {
	prev := validate.AllowPrivateIPs.Load()
	validate.AllowPrivateIPs.Store(true)
	t.Cleanup(func() { validate.AllowPrivateIPs.Store(prev) })

	pub := &pkgfedtest.StubPublisher{}
	originSrv := newTestServerWithPublisher(t, pub)
	originActor := originSrv.URL + "/ap/actor"

	body := publishBody(t, "serde", "1.0.0", nil, []byte("real-crate-bytes"))
	resp := doRequest(t, originSrv, http.MethodPut, "/cargo/api/v1/crates/new", body, true)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()
	originActs := pub.Activities()
	require.Len(t, originActs, 1)

	peerDB, err := database.OpenSQLite(t.TempDir(), 0, 0, nopLog())
	require.NoError(t, err)
	t.Cleanup(func() { _ = peerDB.Close() })
	peerBlobs, err := blobstore.New(t.TempDir(), nopLog())
	require.NoError(t, err)
	require.NoError(t, peerDB.UpsertActor(t.Context(), &database.Actor{
		ActorURL:          originActor,
		Endpoint:          originSrv.URL,
		ReplicationPolicy: "lazy",
		IsHealthy:         true,
	}))

	fetcher := peering.NewFetcher(10*time.Second, 10<<20, 10<<20, nopLog())
	replicator := peering.NewBlobReplicator(peerDB, peerBlobs, fetcher, silentNotifier{}, nopLog())

	peer := New(Config{
		DB:         peerDB,
		Blobs:      peerBlobs,
		Endpoint:   "https://peer.example.com",
		Owner:      "https://peer.example.com/ap/actor",
		Replicator: replicator,
		Logger:     nopLog(),
	})

	raw, err := json.Marshal(originActs[0].Object)
	require.NoError(t, err)
	var asMap map[string]any
	require.NoError(t, json.Unmarshal(raw, &asMap))
	require.NoError(t, peer.FederationAdapter().Ingest(t.Context(), "Create", "CargoVersion", asMap, originActor))
	replicator.Wait()

	pkg, err := peerDB.GetPackage(t.Context(), packageType, "serde")
	require.NoError(t, err)
	require.NotNil(t, pkg)
	v, err := peerDB.GetPackageVersion(t.Context(), pkg.ID, "1.0.0")
	require.NoError(t, err)
	files, err := peerDB.ListPackageFiles(t.Context(), v.ID)
	require.NoError(t, err)
	require.Len(t, files, 1)

	exists, err := peerBlobs.Exists(t.Context(), files[0].BlobDigest)
	require.NoError(t, err)
	assert.True(t, exists, "peer should have replicated the crate bytes locally")

	rc, _, err := peerBlobs.Open(t.Context(), files[0].BlobDigest)
	require.NoError(t, err)
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	assert.Equal(t, []byte("real-crate-bytes"), got)
}

type silentNotifier struct{}

func (silentNotifier) Send(_, _ string) {}

func TestPeerRedirectsToOriginOnBlobMiss(t *testing.T) {
	pub := &pkgfedtest.StubPublisher{}
	originSrv := newTestServerWithPublisher(t, pub)
	originActor := originSrv.URL + "/ap/actor"

	body := publishBody(t, "serde", "1.0.0", nil, []byte("crate"))
	resp := doRequest(t, originSrv, http.MethodPut, "/cargo/api/v1/crates/new", body, true)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()
	originActs := pub.Activities()
	require.Len(t, originActs, 1)

	peerDB, err := database.OpenSQLite(t.TempDir(), 0, 0, nopLog())
	require.NoError(t, err)
	t.Cleanup(func() { _ = peerDB.Close() })
	peerBlobs, err := blobstore.New(t.TempDir(), nopLog())
	require.NoError(t, err)
	require.NoError(t, peerDB.UpsertActor(t.Context(), &database.Actor{
		ActorURL:          originActor,
		Endpoint:          originSrv.URL,
		ReplicationPolicy: "lazy",
		IsHealthy:         true,
	}))

	peerSrv := httptest.NewServer(nil)
	t.Cleanup(peerSrv.Close)
	peer := New(Config{
		DB:       peerDB,
		Blobs:    peerBlobs,
		Endpoint: peerSrv.URL,
		Owner:    peerSrv.URL + "/ap/actor",
		Logger:   nopLog(),
	})
	peerSrv.Config.Handler = peer.Handler()

	raw, err := json.Marshal(originActs[0].Object)
	require.NoError(t, err)
	var asMap map[string]any
	require.NoError(t, json.Unmarshal(raw, &asMap))
	require.NoError(t, peer.FederationAdapter().Ingest(t.Context(), "Create", "CargoVersion", asMap, originActor))

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, err := client.Get(peerSrv.URL + "/cargo/api/v1/crates/serde/1.0.0/download")
	require.NoError(t, err)
	defer func() { _ = r.Body.Close() }()
	assert.Equal(t, http.StatusFound, r.StatusCode)
	assert.Equal(t, originSrv.URL+"/cargo/api/v1/crates/serde/1.0.0/download", r.Header.Get("Location"))
}

func TestAdapterIngestRoundTrip(t *testing.T) {
	pub := &pkgfedtest.StubPublisher{}
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

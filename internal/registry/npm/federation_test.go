package npm

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

func TestPublishEmitsActivities(t *testing.T) {
	pub := &pkgfedtest.StubPublisher{}
	srv := newTestServerWithPublisher(t, pub)

	body := publishBody(t, testPkgLodash, testVersion, []byte("tarball"), "latest")
	resp := doRequest(t, srv, http.MethodPut, "/npm/lodash", body, true)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	_ = resp.Body.Close()

	acts := pub.Activities()
	require.Len(t, acts, 2)
	assert.Equal(t, "Create", acts[0].Type)
	v, ok := acts[0].Object.(NpmVersion)
	require.True(t, ok)
	assert.Equal(t, testPkgLodash, v.NpmName)
	assert.Equal(t, testVersion, v.NpmVersion)
	assert.Contains(t, v.NpmTarball, "/npm/lodash/-/lodash-1.0.0.tgz")
	assert.NotEmpty(t, v.NpmIntegrity)

	assert.Equal(t, "Update", acts[1].Type)
	tag, ok := acts[1].Object.(NpmTag)
	require.True(t, ok)
	assert.Equal(t, testPkgLodash, tag.NpmName)
	assert.Equal(t, "latest", tag.NpmTag)
	assert.Equal(t, testVersion, tag.NpmVersion)
}

func TestDistTagPutAndDeleteEmitActivities(t *testing.T) {
	pub := &pkgfedtest.StubPublisher{}
	srv := newTestServerWithPublisher(t, pub)

	body := publishBody(t, "react", testVersion, []byte("data"), "")
	resp := doRequest(t, srv, http.MethodPut, "/npm/react", body, true)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	_ = resp.Body.Close()

	startCount := len(pub.Activities())

	versionJSON, _ := json.Marshal(testVersion)
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
	pub := &pkgfedtest.StubPublisher{}
	srv := newTestServerWithPublisher(t, pub)
	originActor := srv.URL + "/ap/actor"

	body := publishBody(t, testPkgLodash, testVersion, []byte("tarball"), "latest")
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

	pkg, err := peerDB.GetPackage(t.Context(), packageType, testPkgLodash)
	require.NoError(t, err)
	require.NotNil(t, pkg)
	versions, err := peerDB.ListPackageVersions(t.Context(), pkg.ID)
	require.NoError(t, err)
	require.Len(t, versions, 1)
	assert.Equal(t, testVersion, versions[0].Version)

	tags, err := peerDB.ListPackageTags(t.Context(), pkg.ID)
	require.NoError(t, err)
	require.Len(t, tags, 1)
	assert.Equal(t, "latest", tags[0].Name)
	assert.Equal(t, testVersion, tags[0].Version)
}

func TestEagerReplicationStoresBytesLocally(t *testing.T) {
	prev := validate.AllowPrivateIPs.Load()
	validate.AllowPrivateIPs.Store(true)
	t.Cleanup(func() { validate.AllowPrivateIPs.Store(prev) })

	pub := &pkgfedtest.StubPublisher{}
	originSrv := newTestServerWithPublisher(t, pub)
	originActor := originSrv.URL + "/ap/actor"

	body := publishBody(t, testPkgLodash, testVersion, []byte("real-tarball-bytes"), "")
	resp := doRequest(t, originSrv, http.MethodPut, "/npm/lodash", body, true)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	_ = resp.Body.Close()
	originActs := pub.Activities()
	require.NotEmpty(t, originActs)

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
	adapter := peer.FederationAdapter()

	for _, act := range originActs {
		raw, err := json.Marshal(act.Object)
		require.NoError(t, err)
		var asMap map[string]any
		require.NoError(t, json.Unmarshal(raw, &asMap))
		apType, _ := asMap["type"].(string)
		require.NoError(t, adapter.Ingest(t.Context(), act.Type, apType, asMap, originActor))
	}
	replicator.Wait()

	pkg, err := peerDB.GetPackage(t.Context(), packageType, testPkgLodash)
	require.NoError(t, err)
	require.NotNil(t, pkg)
	v, err := peerDB.GetPackageVersion(t.Context(), pkg.ID, testVersion)
	require.NoError(t, err)
	files, err := peerDB.ListPackageFiles(t.Context(), v.ID)
	require.NoError(t, err)
	require.Len(t, files, 1)

	exists, err := peerBlobs.Exists(t.Context(), files[0].BlobDigest)
	require.NoError(t, err)
	assert.True(t, exists, "peer should have replicated the tarball bytes locally")

	rc, _, err := peerBlobs.Open(t.Context(), files[0].BlobDigest)
	require.NoError(t, err)
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	assert.Equal(t, []byte("real-tarball-bytes"), got)
}

type silentNotifier struct{}

func (silentNotifier) Send(_, _ string) {}

func TestPeerRedirectsToOriginOnBlobMiss(t *testing.T) {
	pub := &pkgfedtest.StubPublisher{}
	originSrv := newTestServerWithPublisher(t, pub)
	originActor := originSrv.URL + "/ap/actor"

	body := publishBody(t, testPkgLodash, testVersion, []byte("tarball"), "")
	resp := doRequest(t, originSrv, http.MethodPut, "/npm/lodash", body, true)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	_ = resp.Body.Close()
	originActs := pub.Activities()
	require.NotEmpty(t, originActs)

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

	for _, act := range originActs {
		raw, err := json.Marshal(act.Object)
		require.NoError(t, err)
		var asMap map[string]any
		require.NoError(t, json.Unmarshal(raw, &asMap))
		apType, _ := asMap["type"].(string)
		require.NoError(t, peer.FederationAdapter().Ingest(t.Context(), act.Type, apType, asMap, originActor))
	}

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, err := client.Get(peerSrv.URL + "/npm/lodash/-/lodash-1.0.0.tgz")
	require.NoError(t, err)
	defer func() { _ = r.Body.Close() }()
	assert.Equal(t, http.StatusFound, r.StatusCode)
	assert.Equal(t, originSrv.URL+"/npm/lodash/-/lodash-1.0.0.tgz", r.Header.Get("Location"))
}

func TestAdapterRejectsForeignSender(t *testing.T) {
	db, err := database.OpenSQLite(t.TempDir(), 0, 0, nopLog())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	blobs, err := blobstore.New(t.TempDir(), nopLog())
	require.NoError(t, err)

	_, err = db.GetOrCreatePackage(t.Context(), packageType, "owned", testOwnerURL)
	require.NoError(t, err)

	b := New(Config{DB: db, Blobs: blobs, Endpoint: "https://test", Owner: "https://test/ap/actor", Logger: nopLog()})
	adapter := b.FederationAdapter()

	err = adapter.Ingest(t.Context(), "Create", "NpmVersion", map[string]any{
		"npmName":    "owned",
		"npmVersion": testVersion,
	}, "https://eve.example.com/ap/actor")
	require.Error(t, err)
	assert.ErrorIs(t, err, database.ErrPackageOwnerMismatch)
}

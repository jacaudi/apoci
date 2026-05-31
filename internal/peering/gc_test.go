package peering

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/blobstore"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/notify"
)

const testManifestDigest = "sha256:m1"

func testGCDeps(t *testing.T) (*database.DB, *blobstore.Store) {
	t.Helper()

	dbDir := t.TempDir()
	db, err := database.OpenSQLite(dbDir, 0, 0, nopLog())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	blobDir := t.TempDir()
	blobs, err := blobstore.New(blobDir, nopLog())
	require.NoError(t, err)

	return db, blobs
}

func insertTestPeer(t *testing.T, ctx context.Context, db *database.DB, actorURL, endpoint string) {
	t.Helper()
	name := "test-peer"
	require.NoError(t, db.UpsertActor(ctx, &database.Actor{
		ActorURL:          actorURL,
		Name:              &name,
		Endpoint:          endpoint,
		ReplicationPolicy: "lazy",
		IsHealthy:         true,
	}))
}

func TestGCCleansStalePeerBlobs(t *testing.T) {
	db, _ := testGCDeps(t)
	ctx := context.Background()

	insertTestPeer(t, ctx, db, "https://stale.example.com/ap/actor", "https://stale.example.com")

	digest := "sha256:aabbccddee000000000000000000000000000000000000000000000000000000"
	require.NoError(t, db.PutPeerBlob(ctx, "https://stale.example.com/ap/actor", digest, "https://stale.example.com"))

	// CleanupStalePeerBlobs computes cutoff = now - olderThan. With a negative duration,
	// cutoff lands slightly in the future, guaranteeing the row is older than the cutoff.
	time.Sleep(10 * time.Millisecond)
	n, err := db.CleanupStalePeerBlobs(ctx, -1*time.Second)
	require.NoError(t, err)
	require.Equal(t, int64(1), n, "expected 1 stale peer blob removed")

	pbs, err := db.FindPeersWithBlob(ctx, digest)
	require.NoError(t, err)
	require.Len(t, pbs, 0)
}

func TestGCCleansOrphanedBlobMetadata(t *testing.T) {
	db, _ := testGCDeps(t)
	ctx := context.Background()

	// Insert a blob that is NOT stored locally and has no peer refs or manifest layers.
	orphanDigest := "sha256:0000000000000000000000000000000000000000000000000000000000000001"
	require.NoError(t, db.PutBlob(ctx, orphanDigest, 100, nil, false))

	digests, err := db.OrphanedBlobs(ctx, 100, time.Time{})
	require.NoError(t, err)
	require.Len(t, digests, 1)
	require.Equal(t, orphanDigest, digests[0])

	require.NoError(t, db.DeleteBlob(ctx, orphanDigest))

	blob, err := db.GetBlob(ctx, orphanDigest)
	require.NoError(t, err)
	require.Nil(t, blob, "expected orphaned blob metadata to be removed")
}

func TestOrphanedBlobs_LocallyStoredWithoutReferences(t *testing.T) {
	db, _ := testGCDeps(t)
	ctx := context.Background()

	// stored_locally=true with no manifest or peer reference: was the bug we fixed.
	digest := "sha256:0000000000000000000000000000000000000000000000000000000000000777"
	require.NoError(t, db.PutBlob(ctx, digest, 100, nil, true))

	digests, err := db.OrphanedBlobs(ctx, 100, time.Time{})
	require.NoError(t, err)
	require.Contains(t, digests, digest)
}

// Local bytes must not be pinned by peer_blobs: that row only records remote
// locations and shouldn't survive after the last manifest reference goes away.
func TestOrphanedBlobs_LocallyStoredIgnoresPeerBlobs(t *testing.T) {
	db, _ := testGCDeps(t)
	ctx := context.Background()

	insertTestPeer(t, ctx, db, "https://peer.example.com/ap/actor", "https://peer.example.com")

	digest := "sha256:00000000000000000000000000000000000000000000000000000000000000aa"
	require.NoError(t, db.PutBlob(ctx, digest, 100, nil, true))
	require.NoError(t, db.PutPeerBlob(ctx, "https://peer.example.com/ap/actor", digest, "https://peer.example.com"))

	digests, err := db.OrphanedBlobs(ctx, 100, time.Time{})
	require.NoError(t, err)
	require.Contains(t, digests, digest)
}

// Federation-metadata rows (stored_locally=false + peer_blobs) are redirect
// targets for OCI pulls — GC must leave them alone.
func TestOrphanedBlobs_RemoteOnlyKeptForRedirect(t *testing.T) {
	db, _ := testGCDeps(t)
	ctx := context.Background()

	insertTestPeer(t, ctx, db, "https://peer.example.com/ap/actor", "https://peer.example.com")

	digest := "sha256:00000000000000000000000000000000000000000000000000000000000000bb"
	require.NoError(t, db.PutBlob(ctx, digest, 100, nil, false))
	require.NoError(t, db.PutPeerBlob(ctx, "https://peer.example.com/ap/actor", digest, "https://peer.example.com"))

	digests, err := db.OrphanedBlobs(ctx, 100, time.Time{})
	require.NoError(t, err)
	require.NotContains(t, digests, digest)
}

func TestOrphanedBlobs_GracePeriod(t *testing.T) {
	db, _ := testGCDeps(t)
	ctx := context.Background()

	digest := "sha256:0000000000000000000000000000000000000000000000000000000000000888"
	require.NoError(t, db.PutBlob(ctx, digest, 100, nil, true))

	// Cutoff in the past: just-inserted blob is preserved.
	digests, err := db.OrphanedBlobs(ctx, 100, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.NotContains(t, digests, digest)

	// Cutoff in the future: blob shows up as orphan.
	digests, err = db.OrphanedBlobs(ctx, 100, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Contains(t, digests, digest)
}

func TestGCCleansOrphanedBlobFiles(t *testing.T) {
	db, blobs := testGCDeps(t)
	ctx := context.Background()

	// Write a blob to disk but do NOT register it in the database.
	digest, _, err := blobs.Put(ctx, strings.NewReader("orphaned blob data on disk"), "")
	require.NoError(t, err)

	exists, err := blobs.Exists(ctx, digest)
	require.NoError(t, err)
	require.True(t, exists, "expected blob file to exist before cleanup")

	// Check that AllBlobDigests returns nothing (blob not in DB).
	knownDigests, err := db.AllBlobDigests(ctx, 1000)
	require.NoError(t, err)
	require.False(t, knownDigests[digest], "expected digest to NOT be in DB")

	// Manually delete the orphaned blob file (simulating what GC should do).
	require.NoError(t, blobs.Delete(ctx, digest))

	gone, err := blobs.Exists(ctx, digest)
	require.NoError(t, err)
	require.False(t, gone, "expected orphaned blob file to be removed")
}

func TestGCPreservesValidData(t *testing.T) {
	db, blobs := testGCDeps(t)
	ctx := context.Background()

	// 1. Recent peer blob (should be preserved by a 30-day cleanup).
	insertTestPeer(t, ctx, db, "https://recent.example.com/ap/actor", "https://recent.example.com")

	recentDigest := "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	require.NoError(t, db.PutPeerBlob(ctx, "https://recent.example.com/ap/actor", recentDigest, "https://recent.example.com"))

	// Cleanup with 30-day threshold should NOT remove the recent peer blob.
	n, err := db.CleanupStalePeerBlobs(ctx, 30*24*time.Hour)
	require.NoError(t, err)
	require.Equal(t, int64(0), n)

	pbs, err := db.FindPeersWithBlob(ctx, recentDigest)
	require.NoError(t, err)
	require.Len(t, pbs, 1)

	// 2. Locally stored blob referenced by a manifest is NOT an orphan.
	pkg, err := db.GetOrCreatePackage(ctx, "oci", "foo.com/img", "https://alice.example.com/ap/actor")
	require.NoError(t, err)
	v := &database.PackageVersion{PackageID: pkg.ID, Version: testManifestDigest, Metadata: []byte(`{}`)}
	require.NoError(t, db.PutPackageVersion(ctx, v))
	localDigest := "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	require.NoError(t, db.PutBlob(ctx, localDigest, 200, nil, true))
	require.NoError(t, db.PutBlobReferences(ctx, v.ID, []database.BlobRef{{Digest: localDigest, Size: 200}}))

	orphans, err := db.OrphanedBlobs(ctx, 100, time.Time{})
	require.NoError(t, err)
	for _, d := range orphans {
		require.NotEqual(t, localDigest, d, "expected referenced local blob to NOT be orphaned")
	}

	// 3. Blob file on disk with a matching DB record should be preserved.
	diskDigest, _, err := blobs.Put(ctx, strings.NewReader("valid blob on disk"), "")
	require.NoError(t, err)
	require.NoError(t, db.PutBlob(ctx, diskDigest, 18, nil, true))

	knownDigests, err := db.AllBlobDigests(ctx, 1000)
	require.NoError(t, err)
	require.True(t, knownDigests[diskDigest], "expected disk blob digest to be in known digests")

	diskExists, err := blobs.Exists(ctx, diskDigest)
	require.NoError(t, err)
	require.True(t, diskExists, "expected valid blob file to remain on disk")
}

func newReconcileGC(t *testing.T, db *database.DB, blobs *blobstore.Store) *GarbageCollector {
	t.Helper()
	return NewGarbageCollector(GCConfig{
		Interval:         6 * time.Hour,
		StalePeerBlobAge: 30 * 24 * time.Hour,
		OrphanBatchSize:  500,
	}, db, blobs, notify.New("test", nil, nil, nopLog()), nopLog())
}

func TestReconcile_DegradesWhenFileMissingAndPeerHasIt(t *testing.T) {
	db, blobs := testGCDeps(t)
	ctx := context.Background()
	gc := newReconcileGC(t, db, blobs)

	insertTestPeer(t, ctx, db, "https://peer.example.com/ap/actor", "https://peer.example.com")

	digest := "sha256:00000000000000000000000000000000000000000000000000000000000000c1"
	require.NoError(t, db.PutBlob(ctx, digest, 100, nil, true)) // claims local but file does not exist
	require.NoError(t, db.PutPeerBlob(ctx, "https://peer.example.com/ap/actor", digest, "https://peer.example.com"))

	gc.reconcileBlobStorageDrift(ctx)

	got, err := db.GetBlob(ctx, digest)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.False(t, got.StoredLocally, "stored_locally should have been flipped to false")
}

func TestReconcile_PromotesWhenFilePresentAndRowSaysRemote(t *testing.T) {
	db, blobs := testGCDeps(t)
	ctx := context.Background()
	gc := newReconcileGC(t, db, blobs)

	digest, size, err := blobs.Put(ctx, strings.NewReader("recoverable bytes"), "")
	require.NoError(t, err)
	require.NoError(t, db.PutBlob(ctx, digest, size, nil, false))

	gc.reconcileBlobStorageDrift(ctx)

	got, err := db.GetBlob(ctx, digest)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.True(t, got.StoredLocally, "stored_locally should have been flipped to true")
}

func TestReconcile_LeavesRowAloneWhenNoPeerAndFileMissing(t *testing.T) {
	db, blobs := testGCDeps(t)
	ctx := context.Background()
	gc := newReconcileGC(t, db, blobs)

	digest := "sha256:00000000000000000000000000000000000000000000000000000000000000c2"
	require.NoError(t, db.PutBlob(ctx, digest, 100, nil, true))

	gc.reconcileBlobStorageDrift(ctx)

	got, err := db.GetBlob(ctx, digest)
	require.NoError(t, err)
	require.NotNil(t, got, "unrecoverable drift must not silently delete the row")
	require.True(t, got.StoredLocally)
}

func TestOrphanedBlobFiles_RepairsRowWhenManifestStillReferences(t *testing.T) {
	db, blobs := testGCDeps(t)
	ctx := context.Background()
	gc := newReconcileGC(t, db, blobs)

	pkg, err := db.GetOrCreatePackage(ctx, "oci", "example/img", "https://alice.example.com/ap/actor")
	require.NoError(t, err)
	v := &database.PackageVersion{PackageID: pkg.ID, Version: testManifestDigest, Metadata: []byte(`{}`)}
	require.NoError(t, db.PutPackageVersion(ctx, v))

	// Write bytes to disk but leave the blobs row out — simulates a DB restore
	// older than the on-disk store.
	digest, size, err := blobs.Put(ctx, strings.NewReader("orphan but referenced"), "")
	require.NoError(t, err)
	require.NoError(t, db.PutBlobReferences(ctx, v.ID, []database.BlobRef{{Digest: digest, Size: size}}))

	require.Nil(t, mustGetBlob(t, ctx, db, digest), "precondition: blobs row should not exist")

	gc.cleanupOrphanedBlobFiles(ctx)

	got := mustGetBlob(t, ctx, db, digest)
	require.NotNil(t, got, "blobs row should have been repaired")
	require.Equal(t, size, got.SizeBytes)
	require.True(t, got.StoredLocally)

	exists, err := blobs.Exists(ctx, digest)
	require.NoError(t, err)
	require.True(t, exists, "the file must not be deleted when repair was performed")
}

func mustGetBlob(t *testing.T, ctx context.Context, db *database.DB, digest string) *database.Blob {
	t.Helper()
	b, err := db.GetBlob(ctx, digest)
	require.NoError(t, err)
	return b
}

func TestGCStartStop(t *testing.T) {
	db, blobs := testGCDeps(t)
	ctx := context.Background()

	gc := NewGarbageCollector(GCConfig{
		Interval:         6 * time.Hour,
		StalePeerBlobAge: 30 * 24 * time.Hour,
		OrphanBatchSize:  500,
	}, db, blobs, notify.New("test", nil, nil, nopLog()), nopLog())
	gc.Start(ctx)

	// Stop should return promptly without panic.
	gc.Stop()
}

func TestGCStatus(t *testing.T) {
	db, blobs := testGCDeps(t)
	ctx := context.Background()

	gc := NewGarbageCollector(GCConfig{
		Interval:         6 * time.Hour,
		StalePeerBlobAge: 30 * 24 * time.Hour,
		OrphanBatchSize:  500,
	}, db, blobs, notify.New("test", nil, nil, nopLog()), nopLog())

	st := gc.Status()
	require.False(t, st.Running)
	require.Nil(t, st.LastRun)
	require.Equal(t, (6 * time.Hour).Milliseconds(), st.IntervalMs)

	gc.RunOnce(ctx)

	st = gc.Status()
	require.False(t, st.Running)
	require.NotNil(t, st.LastRun)
	require.NotNil(t, st.NextRunEstimate)
	require.Equal(t, st.LastRun.Add(6*time.Hour), *st.NextRunEstimate)
}

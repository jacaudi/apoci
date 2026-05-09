package peering

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/notify"
)

const testActor = "https://alice.example.com/ap/actor"

type recordingPublisher struct {
	mu      sync.Mutex
	tagDels []string
	manDels []string
}

func (p *recordingPublisher) PublishTagDelete(_ context.Context, repo, tag string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tagDels = append(p.tagDels, repo+":"+tag)
	return nil
}

func (p *recordingPublisher) PublishManifestDelete(_ context.Context, repo, digest string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.manDels = append(p.manDels, repo+"@"+digest)
	return nil
}

func TestRetentionSweep_KeepLastN(t *testing.T) {
	db, blobs := testGCDeps(t)
	ctx := context.Background()

	pkg, err := db.GetOrCreatePackage(ctx, "oci", "foo.com/img", testActor)
	require.NoError(t, err)

	// Put 5 tags, then backdate updated_at deterministically so retention has a
	// stable order to choose by.
	now := time.Now()
	tags := []string{"oldest", "second", "third", "fourth", "newest"}
	for i, name := range tags {
		dgst := "sha256:" + name
		require.NoError(t, db.PutPackageVersion(ctx, &database.PackageVersion{
			PackageID: pkg.ID, Version: dgst, Metadata: []byte(`{}`),
		}))
		require.NoError(t, db.PutPackageTag(ctx, pkg.ID, name, dgst, false))
		_, err := db.ExecContext(ctx,
			"UPDATE package_tags SET updated_at = ? WHERE package_id = ? AND name = ?",
			now.Add(time.Duration(i)*time.Minute), pkg.ID, name)
		require.NoError(t, err)
	}

	pub := &recordingPublisher{}
	gc := NewGarbageCollector(GCConfig{
		Interval:              6 * time.Hour,
		StalePeerBlobAge:      30 * 24 * time.Hour,
		OrphanBatchSize:       500,
		BlobGCGracePeriod:     time.Hour,
		RetentionDefaults:     RetentionPolicy{KeepLastN: 2},
		RetentionTagsPerCycle: 100,
	}, db, blobs, notify.New("test", nil, nil, nopLog()), nopLog())
	gc.SetFederationPublisher(pub)
	gc.RunOnce(ctx)

	left, err := db.ListPackageTags(ctx, pkg.ID)
	require.NoError(t, err)
	names := make([]string, 0, len(left))
	for _, t := range left {
		names = append(names, t.Name)
	}
	require.ElementsMatch(t, []string{"newest", "fourth"}, names)
	require.Len(t, pub.tagDels, 3)
}

func TestRetentionSweep_PinnedAndImmutable(t *testing.T) {
	db, blobs := testGCDeps(t)
	ctx := context.Background()

	pkg, err := db.GetOrCreatePackage(ctx, "oci", "foo.com/img", testActor)
	require.NoError(t, err)

	now := time.Now()
	// Put pinned + immutable + three plain tags with deterministic ordering.
	specs := []struct {
		name      string
		immutable bool
		offset    int
	}{
		{"latest", false, 5},
		{"v1.0", true, 4},
		{"old1", false, 3},
		{"old2", false, 2},
		{"old3", false, 1},
	}
	for _, s := range specs {
		dgst := "sha256:" + s.name
		require.NoError(t, db.PutPackageVersion(ctx, &database.PackageVersion{
			PackageID: pkg.ID, Version: dgst, Metadata: []byte(`{}`),
		}))
		require.NoError(t, db.PutPackageTag(ctx, pkg.ID, s.name, dgst, s.immutable))
		_, err := db.ExecContext(ctx,
			"UPDATE package_tags SET updated_at = ? WHERE package_id = ? AND name = ?",
			now.Add(time.Duration(s.offset)*time.Minute), pkg.ID, s.name)
		require.NoError(t, err)
	}

	gc := NewGarbageCollector(GCConfig{
		Interval:              6 * time.Hour,
		StalePeerBlobAge:      30 * 24 * time.Hour,
		OrphanBatchSize:       500,
		BlobGCGracePeriod:     time.Hour,
		RetentionDefaults:     RetentionPolicy{KeepLastN: 1, PinnedGlobs: []string{"latest"}},
		RetentionTagsPerCycle: 100,
	}, db, blobs, notify.New("test", nil, nil, nopLog()), nopLog())
	gc.RunOnce(ctx)

	left, err := db.ListPackageTags(ctx, pkg.ID)
	require.NoError(t, err)
	names := make([]string, 0, len(left))
	for _, t := range left {
		names = append(names, t.Name)
	}
	// latest pinned, v1.0 immutable, KeepLastN=1 keeps the most recent of the rest.
	require.Contains(t, names, "latest")
	require.Contains(t, names, "v1.0")
	require.Contains(t, names, "old1") // most recent among non-pinned mutable tags
	require.Len(t, names, 3)
}

func TestPruneUntaggedManifestsGC_FederatesDelete(t *testing.T) {
	db, blobs := testGCDeps(t)
	ctx := context.Background()

	pkg, err := db.GetOrCreatePackage(ctx, "oci", "foo.com/img", testActor)
	require.NoError(t, err)

	require.NoError(t, db.PutPackageVersion(ctx, &database.PackageVersion{
		PackageID: pkg.ID, Version: "sha256:gone", Metadata: []byte(`{}`),
	}))
	// Backdate so the prune cutoff (now - 1h) is past the row's created_at.
	_, err = db.ExecContext(ctx,
		"UPDATE package_versions SET created_at = ? WHERE package_id = ?",
		time.Now().Add(-2*time.Hour), pkg.ID)
	require.NoError(t, err)

	pub := &recordingPublisher{}
	gc := NewGarbageCollector(GCConfig{
		Interval:            6 * time.Hour,
		StalePeerBlobAge:    30 * 24 * time.Hour,
		OrphanBatchSize:     500,
		BlobGCGracePeriod:   time.Hour,
		UntaggedManifestAge: time.Hour,
		UntaggedBatchSize:   100,
	}, db, blobs, notify.New("test", nil, nil, nopLog()), nopLog())
	gc.SetFederationPublisher(pub)
	gc.RunOnce(ctx)

	require.Len(t, pub.manDels, 1)
	require.True(t, strings.Contains(pub.manDels[0], "sha256:gone"))

	gone, err := db.GetPackageVersion(ctx, pkg.ID, "sha256:gone")
	require.NoError(t, err)
	require.Nil(t, gone)
}

func TestCleanupOrphanedBlobFiles_GraceWindow(t *testing.T) {
	db, blobs := testGCDeps(t)
	ctx := context.Background()

	digest, _, err := blobs.Put(ctx, strings.NewReader("recent"), "")
	require.NoError(t, err)

	gc := NewGarbageCollector(GCConfig{
		Interval:          6 * time.Hour,
		StalePeerBlobAge:  30 * 24 * time.Hour,
		OrphanBatchSize:   500,
		BlobGCGracePeriod: time.Hour,
	}, db, blobs, notify.New("test", nil, nil, nopLog()), nopLog())
	gc.RunOnce(ctx)

	exists, err := blobs.Exists(ctx, digest)
	require.NoError(t, err)
	require.True(t, exists, "blob within grace window should be preserved")

	// Re-run with grace disabled: the orphan should now be deleted.
	gc2 := NewGarbageCollector(GCConfig{
		Interval:          6 * time.Hour,
		StalePeerBlobAge:  30 * 24 * time.Hour,
		OrphanBatchSize:   500,
		BlobGCGracePeriod: 0,
	}, db, blobs, notify.New("test", nil, nil, nopLog()), nopLog())
	gc2.RunOnce(ctx)

	gone, err := blobs.Exists(ctx, digest)
	require.NoError(t, err)
	require.False(t, gone, "blob should be deleted when grace is disabled")
}

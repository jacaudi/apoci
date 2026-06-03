package database

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPruneUntaggedManifests_DropsOldUntagged(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	pkg, err := db.GetOrCreatePackage(ctx, ociPackageType, "foo.com/img", testAliceActor)
	require.NoError(t, err)

	// One tagged version, one untagged.
	tagged := &PackageVersion{PackageID: pkg.ID, Version: "sha256:tagged", Metadata: []byte(`{}`)}
	untagged := &PackageVersion{PackageID: pkg.ID, Version: "sha256:untagged", Metadata: []byte(`{}`)}
	require.NoError(t, db.PutPackageVersion(ctx, tagged))
	require.NoError(t, db.PutPackageVersion(ctx, untagged))
	require.NoError(t, db.PutPackageTag(ctx, pkg.ID, "latest", "sha256:tagged"))

	// Backdate the untagged version's created_at to be older than the cutoff.
	_, err = db.bun.NewRaw(
		"UPDATE package_versions SET created_at = ? WHERE id = ?",
		time.Now().Add(-2*time.Hour), untagged.ID,
	).Exec(ctx)
	require.NoError(t, err)

	rows, err := db.PruneUntaggedManifests(ctx, time.Hour, 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "sha256:untagged", rows[0].Digest)
	require.Equal(t, "foo.com/img", rows[0].PackageName)

	// The tagged version is still there.
	got, err := db.GetPackageVersion(ctx, pkg.ID, "sha256:tagged")
	require.NoError(t, err)
	require.NotNil(t, got)

	// The untagged version is gone.
	gone, err := db.GetPackageVersion(ctx, pkg.ID, "sha256:untagged")
	require.NoError(t, err)
	require.Nil(t, gone)
}

func TestPruneUntaggedManifests_RespectsAge(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	pkg, err := db.GetOrCreatePackage(ctx, ociPackageType, "foo.com/young", testAliceActor)
	require.NoError(t, err)
	v := &PackageVersion{PackageID: pkg.ID, Version: "sha256:young", Metadata: []byte(`{}`)}
	require.NoError(t, db.PutPackageVersion(ctx, v))

	// Untagged but recent — should be preserved by 1h cutoff.
	rows, err := db.PruneUntaggedManifests(ctx, time.Hour, 10)
	require.NoError(t, err)
	require.Empty(t, rows)

	got, err := db.GetPackageVersion(ctx, pkg.ID, "sha256:young")
	require.NoError(t, err)
	require.NotNil(t, got)
}

func TestPruneUntaggedManifests_PreservesReferrerSubject(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	pkg, err := db.GetOrCreatePackage(ctx, ociPackageType, "foo.com/sigs", testAliceActor)
	require.NoError(t, err)

	subjectDigest := "sha256:subject"
	subject := &PackageVersion{PackageID: pkg.ID, Version: subjectDigest, Metadata: []byte(`{}`)}
	require.NoError(t, db.PutPackageVersion(ctx, subject))

	subjPtr := subjectDigest
	referrer := &PackageVersion{
		PackageID:     pkg.ID,
		Version:       "sha256:referrer",
		Metadata:      []byte(`{}`),
		SubjectDigest: &subjPtr,
	}
	require.NoError(t, db.PutPackageVersion(ctx, referrer))

	// Backdate both so they're prune-eligible by age.
	_, err = db.bun.NewRaw(
		"UPDATE package_versions SET created_at = ? WHERE package_id = ?",
		time.Now().Add(-2*time.Hour), pkg.ID,
	).Exec(ctx)
	require.NoError(t, err)

	// Neither has a tag. Subject is referenced by the referrer's subject_digest,
	// so it must survive. The referrer itself has no protector — it gets pruned.
	rows, err := db.PruneUntaggedManifests(ctx, time.Hour, 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "sha256:referrer", rows[0].Digest)

	stillThere, err := db.GetPackageVersion(ctx, pkg.ID, subjectDigest)
	require.NoError(t, err)
	require.NotNil(t, stillThere, "subject must survive while a referrer references it")
}

func TestPruneUntaggedManifests_PreservesIndexChildren(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	pkg, err := db.GetOrCreatePackage(ctx, ociPackageType, "foo.com/multiarch", testAliceActor)
	require.NoError(t, err)

	amd := &PackageVersion{PackageID: pkg.ID, Version: "sha256:amd64", Metadata: []byte(`{}`)}
	arm := &PackageVersion{PackageID: pkg.ID, Version: "sha256:arm64", Metadata: []byte(`{}`)}
	idx := &PackageVersion{PackageID: pkg.ID, Version: "sha256:index", Metadata: []byte(`{}`)}
	require.NoError(t, db.PutPackageVersion(ctx, amd))
	require.NoError(t, db.PutPackageVersion(ctx, arm))
	require.NoError(t, db.PutPackageVersion(ctx, idx))
	require.NoError(t, db.PutPackageTag(ctx, pkg.ID, "v1", idx.Version))
	require.NoError(t, db.PutManifestLayers(ctx, idx.ID, []BlobRef{
		{Digest: amd.Version, Size: 1},
		{Digest: arm.Version, Size: 1},
	}))

	_, err = db.bun.NewRaw(
		"UPDATE package_versions SET created_at = ? WHERE package_id = ?",
		time.Now().Add(-2*time.Hour), pkg.ID,
	).Exec(ctx)
	require.NoError(t, err)

	rows, err := db.PruneUntaggedManifests(ctx, time.Hour, 10)
	require.NoError(t, err)
	require.Empty(t, rows, "no manifests should be pruned: index is tagged, children are referenced")

	for _, v := range []string{amd.Version, arm.Version, idx.Version} {
		got, err := db.GetPackageVersion(ctx, pkg.ID, v)
		require.NoError(t, err)
		require.NotNil(t, got, "%s should survive", v)
	}
}

func TestUpdateFollowFilter_SetAndClear(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	actorURL := testPeerActorURL
	require.NoError(t, db.AddFollow(ctx, actorURL, "PEM", "https://peer.example.com", nil))

	require.NoError(t, db.UpdateFollowFilter(ctx, actorURL, []string{testTagLatest, "v*"}))
	a, err := db.GetFollow(ctx, actorURL)
	require.NoError(t, err)
	require.NotNil(t, a.FederationTagGlobs)
	require.Equal(t, "latest,v*", *a.FederationTagGlobs)

	// Clear by passing nil/empty.
	require.NoError(t, db.UpdateFollowFilter(ctx, actorURL, nil))
	a, err = db.GetFollow(ctx, actorURL)
	require.NoError(t, err)
	require.Nil(t, a.FederationTagGlobs)
}

func TestUpdateFollowFilter_RejectsBadGlob(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	actorURL := testPeerActorURL
	require.NoError(t, db.AddFollow(ctx, actorURL, "PEM", "https://peer.example.com", nil))

	// path.Match returns ErrBadPattern for unmatched [.
	err := db.UpdateFollowFilter(ctx, actorURL, []string{"v[1.0"})
	require.Error(t, err)
}

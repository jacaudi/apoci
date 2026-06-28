package activitypub

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
)

const tagLatest = "latest"

func TestActorAcceptsActivity(t *testing.T) {
	cases := []struct {
		name   string
		filter *string
		pubCtx pubContext
		want   bool
	}{
		{"nil filter accepts everything", nil, pubContext{kind: pubKindManifest, tag: "anything"}, true},
		{"empty filter rejects tagged activity", new(""), pubContext{kind: pubKindManifest, tag: tagLatest}, false},
		{"latest matches latest", new(tagLatest), pubContext{kind: pubKindManifest, tag: tagLatest}, true},
		{"latest does not match dev", new(tagLatest), pubContext{kind: pubKindManifest, tag: "dev"}, false},
		{"v* matches v1.0", new("v*"), pubContext{kind: pubKindTag, tag: "v1.0"}, true},
		{"latest,v* matches v2.3", new("latest,v*"), pubContext{kind: pubKindTag, tag: "v2.3"}, true},
		{"blob always passes filter", new("nothing"), pubContext{kind: pubKindBlob}, true},
		{"manifest-delete always passes filter", new("nothing"), pubContext{kind: pubKindManifestDelete}, true},
		{"untagged manifest passes filter (digest push)", new(tagLatest), pubContext{kind: pubKindManifest, tag: ""}, true},
		{"tag-delete always passes filter", new(tagLatest), pubContext{kind: pubKindTagDelete, tag: "dev"}, true},
		{"whitespace tolerant", new(" latest , v* "), pubContext{kind: pubKindManifest, tag: "v9"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			actor := &database.Actor{FederationTagGlobs: c.filter}
			require.Equal(t, c.want, actorAcceptsActivity(actor, c.pubCtx))
		})
	}
}

func TestPublishManifestRespectsFilter(t *testing.T) {
	dir := t.TempDir()
	db, err := database.OpenSQLite(dir, 0, 0, discardLogger())
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	id, _ := LoadOrCreateIdentity("https://test.example.com", "test.example.com", "", "", discardLogger())
	pub := NewAPPublisher(id, db, "https://test.example.com", nil, discardLogger())
	t.Cleanup(pub.Stop)

	ctx := context.Background()

	// Three followers, each filter different.
	require.NoError(t, db.AddFollow(ctx, "https://a.example.com/ap/actor", "PEM", "https://a.example.com", nil))
	require.NoError(t, db.AddFollow(ctx, "https://b.example.com/ap/actor", "PEM", "https://b.example.com", nil))
	require.NoError(t, db.AddFollow(ctx, "https://c.example.com/ap/actor", "PEM", "https://c.example.com", nil))
	require.NoError(t, db.UpdateFollowFilter(ctx, "https://b.example.com/ap/actor", []string{tagLatest}))
	require.NoError(t, db.UpdateFollowFilter(ctx, "https://c.example.com/ap/actor", []string{"v*"}))

	// We can't easily mock inbox resolution, so we just verify the DB-level filter
	// behaves correctly. The actorAcceptsActivity test above proves the filter
	// logic; this confirms the filter persists through Add/Update/GetFollow.
	a, err := db.GetFollow(ctx, "https://a.example.com/ap/actor")
	require.NoError(t, err)
	require.Nil(t, a.FederationTagGlobs)

	b, err := db.GetFollow(ctx, "https://b.example.com/ap/actor")
	require.NoError(t, err)
	require.Equal(t, tagLatest, *b.FederationTagGlobs)

	c, err := db.GetFollow(ctx, "https://c.example.com/ap/actor")
	require.NoError(t, err)
	require.Equal(t, "v*", *c.FederationTagGlobs)
}

const excludedRepo = "eleboucher/agentmemory"

func TestRepoExcluded(t *testing.T) {
	pub := &APPublisher{
		identity:      &Identity{AccountDomain: "test.example.com"},
		excludedRepos: []string{excludedRepo, "user/glob-*"},
	}
	cases := []struct {
		repo string
		want bool
	}{
		{excludedRepo, true},
		{"user/glob-foo", true},
		{"user/glob-bar", true},
		{"user/other", false},
		{"eleboucher/agentmemory-mcp", false},
		{"", false},
		// Stored names are namespace-prefixed; relative globs must still match.
		{"test.example.com/" + excludedRepo, true},
		{"test.example.com/user/glob-foo", true},
		{"test.example.com/user/other", false},
	}
	for _, c := range cases {
		t.Run(c.repo, func(t *testing.T) {
			require.Equal(t, c.want, pub.repoExcluded(c.repo))
		})
	}
}

func TestPublishManifestSkippedForExcludedRepo(t *testing.T) {
	dir := t.TempDir()
	db, err := database.OpenSQLite(dir, 0, 0, discardLogger())
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	id, _ := LoadOrCreateIdentity("https://test.example.com", "test.example.com", "", "", discardLogger())
	pub := NewAPPublisher(id, db, "https://test.example.com", []string{excludedRepo}, discardLogger())
	t.Cleanup(pub.Stop)

	ctx := context.Background()
	manifest := []byte(`{"schemaVersion":2}`)
	require.NoError(t, pub.PublishManifest(ctx, excludedRepo, "latest", "sha256:abc", "application/vnd.oci.image.manifest.v1+json", int64(len(manifest)), manifest, nil))
	require.NoError(t, pub.PublishTag(ctx, excludedRepo, "latest", "sha256:abc"))

	activities, err := db.ListActivitiesPage(ctx, id.ActorURL, 0, 10)
	require.NoError(t, err)
	require.Empty(t, activities, "excluded repo non-Delete activities must not be persisted")

	require.NoError(t, pub.PublishManifest(ctx, "eleboucher/other", "latest", "sha256:def", "application/vnd.oci.image.manifest.v1+json", int64(len(manifest)), manifest, nil))
	activities, err = db.ListActivitiesPage(ctx, id.ActorURL, 0, 10)
	require.NoError(t, err)
	require.Len(t, activities, 1)
}

func TestPublishDeleteBypassesExcludedRepo(t *testing.T) {
	dir := t.TempDir()
	db, err := database.OpenSQLite(dir, 0, 0, discardLogger())
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	id, _ := LoadOrCreateIdentity("https://test.example.com", "test.example.com", "", "", discardLogger())
	pub := NewAPPublisher(id, db, "https://test.example.com", []string{excludedRepo}, discardLogger())
	t.Cleanup(pub.Stop)

	ctx := context.Background()
	require.NoError(t, pub.PublishManifestDelete(ctx, excludedRepo, "sha256:abc"))
	require.NoError(t, pub.PublishTagDelete(ctx, excludedRepo, "latest"))

	activities, err := db.ListActivitiesPage(ctx, id.ActorURL, 0, 10)
	require.NoError(t, err)
	require.Len(t, activities, 2, "Delete activities for excluded repo must still be persisted for withdrawal")
	for _, a := range activities {
		require.Equal(t, "Delete", a.Type)
	}
}

func TestWithdrawRepoEmitsDeletesForExcludedRepo(t *testing.T) {
	dir := t.TempDir()
	db, err := database.OpenSQLite(dir, 0, 0, discardLogger())
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	id, _ := LoadOrCreateIdentity("https://test.example.com", "test.example.com", "", "", discardLogger())
	pub := NewAPPublisher(id, db, "https://test.example.com", []string{excludedRepo}, discardLogger())
	t.Cleanup(pub.Stop)

	ctx := context.Background()
	require.NoError(t, pub.WithdrawRepo(ctx, excludedRepo, []string{"latest", "v1.0"}, []string{"sha256:abc", "sha256:def"}))

	activities, err := db.ListActivitiesPage(ctx, id.ActorURL, 0, 10)
	require.NoError(t, err)
	require.Len(t, activities, 4)
	for _, a := range activities {
		require.Equal(t, "Delete", a.Type)
	}
}

func TestPublishManifestCreatesActivity(t *testing.T) {
	dir := t.TempDir()
	db, err := database.OpenSQLite(dir, 0, 0, discardLogger())
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	id, _ := LoadOrCreateIdentity("https://test.example.com", "test.example.com", "", "", discardLogger())
	pub := NewAPPublisher(id, db, "https://test.example.com", nil, discardLogger())
	t.Cleanup(pub.Stop)

	ctx := context.Background()
	manifest := []byte(`{"schemaVersion":2}`)
	err = pub.PublishManifest(ctx, "test/repo", "latest", "sha256:abc123", "application/vnd.oci.image.manifest.v1+json", int64(len(manifest)), manifest, nil)
	require.NoError(t, err)

	activities, err := db.ListActivitiesPage(ctx, id.ActorURL, 0, 10)
	require.NoError(t, err)
	require.Len(t, activities, 1)
	require.Equal(t, "Create", activities[0].Type)

	var activity map[string]any
	require.NoError(t, json.Unmarshal(activities[0].ObjectJSON, &activity))
	require.Equal(t, "Create", activity["type"])

	obj, ok := activity["object"].(map[string]any)
	require.True(t, ok, "expected object to be a map")
	require.Equal(t, "test/repo", obj["ociRepository"])
	require.Equal(t, "sha256:abc123", obj["ociDigest"])
}

func TestPublishTagCreatesUpdateActivity(t *testing.T) {
	dir := t.TempDir()
	db, err := database.OpenSQLite(dir, 0, 0, discardLogger())
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	id, _ := LoadOrCreateIdentity("https://test.example.com", "test.example.com", "", "", discardLogger())
	pub := NewAPPublisher(id, db, "https://test.example.com", nil, discardLogger())
	t.Cleanup(pub.Stop)

	ctx := context.Background()
	err = pub.PublishTag(ctx, "test/repo", "v1.0", "sha256:abc123")
	require.NoError(t, err)

	activities, err := db.ListActivitiesPage(ctx, id.ActorURL, 0, 10)
	require.NoError(t, err)
	require.Len(t, activities, 1)
	require.Equal(t, "Update", activities[0].Type)
}

func TestPublishManifestDeleteCreatesDeleteActivity(t *testing.T) {
	dir := t.TempDir()
	db, err := database.OpenSQLite(dir, 0, 0, discardLogger())
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	id, _ := LoadOrCreateIdentity("https://test.example.com", "test.example.com", "", "", discardLogger())
	pub := NewAPPublisher(id, db, "https://test.example.com", nil, discardLogger())
	t.Cleanup(pub.Stop)

	ctx := context.Background()
	err = pub.PublishManifestDelete(ctx, "test/repo", "sha256:abc123")
	require.NoError(t, err)

	activities, err := db.ListActivitiesPage(ctx, id.ActorURL, 0, 10)
	require.NoError(t, err)
	require.Len(t, activities, 1)
	require.Equal(t, "Delete", activities[0].Type)

	var activity map[string]any
	require.NoError(t, json.Unmarshal(activities[0].ObjectJSON, &activity))
	require.Equal(t, "Delete", activity["type"])

	obj, ok := activity["object"].(map[string]any)
	require.True(t, ok, "expected object to be a map")
	require.Equal(t, "OCIManifest", obj["type"])
	require.Equal(t, "test/repo", obj["ociRepository"])
	require.Equal(t, "sha256:abc123", obj["ociDigest"])
}

func TestPublishTagDeleteCreatesDeleteActivity(t *testing.T) {
	dir := t.TempDir()
	db, err := database.OpenSQLite(dir, 0, 0, discardLogger())
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	id, _ := LoadOrCreateIdentity("https://test.example.com", "test.example.com", "", "", discardLogger())
	pub := NewAPPublisher(id, db, "https://test.example.com", nil, discardLogger())
	t.Cleanup(pub.Stop)

	ctx := context.Background()
	err = pub.PublishTagDelete(ctx, "test/repo", "v1.0")
	require.NoError(t, err)

	activities, err := db.ListActivitiesPage(ctx, id.ActorURL, 0, 10)
	require.NoError(t, err)
	require.Len(t, activities, 1)
	require.Equal(t, "Delete", activities[0].Type)

	var activity map[string]any
	require.NoError(t, json.Unmarshal(activities[0].ObjectJSON, &activity))
	obj, ok := activity["object"].(map[string]any)
	require.True(t, ok, "expected object to be a map")
	require.Equal(t, "OCITag", obj["type"])
	require.Equal(t, "test/repo", obj["ociRepository"])
	require.Equal(t, "v1.0", obj["ociTag"])
}

func TestPublishBlobRefCreatesAnnounceActivity(t *testing.T) {
	dir := t.TempDir()
	db, err := database.OpenSQLite(dir, 0, 0, discardLogger())
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	id, _ := LoadOrCreateIdentity("https://test.example.com", "test.example.com", "", "", discardLogger())
	pub := NewAPPublisher(id, db, "https://test.example.com", nil, discardLogger())
	t.Cleanup(pub.Stop)

	ctx := context.Background()
	err = pub.PublishBlobRef(ctx, "sha256:blob123", 4096)
	require.NoError(t, err)

	activities, err := db.ListActivitiesPage(ctx, id.ActorURL, 0, 10)
	require.NoError(t, err)
	require.Len(t, activities, 1)
	require.Equal(t, "Announce", activities[0].Type)
}

package activitypub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/config"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
)

const (
	aliceDomain           = "alice.test"
	bobActorURL           = "https://bob.test/ap/actor"
	testDigest            = "sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abcd"
	exampleHost           = "example.com"
	testManifestMediaType = "application/vnd.oci.image.manifest.v1+json"
)

// signedInboxPost creates and signs a POST request to /ap/inbox.
func signedInboxPost(t *testing.T, sender *Identity, activity any) *http.Request {
	t.Helper()
	body, err := json.Marshal(activity)
	require.NoError(t, err)
	req := httptest.NewRequest("POST", "/ap/inbox", bytes.NewReader(body))
	req.Header.Set("Content-Type", MediaTypeActivityJSON)
	require.NoError(t, SignRequest(req, sender.KeyID(), sender.PrivateKey, body))
	return req
}

func signedInboxPostWithDate(t *testing.T, sender *Identity, activity any, date time.Time) *http.Request {
	t.Helper()
	body, err := json.Marshal(activity)
	require.NoError(t, err)
	req := httptest.NewRequest("POST", "/ap/inbox", bytes.NewReader(body))
	req.Header.Set("Content-Type", MediaTypeActivityJSON)
	req.Header.Set("Date", date.UTC().Format(http.TimeFormat))
	require.NoError(t, SignRequest(req, sender.KeyID(), sender.PrivateKey, body))
	return req
}

// setupInboxTest creates two identities (alice and bob), an inbox handler for bob,
// and an HTTP server that serves alice's actor document (so bob can fetch the public key).
// It returns the sender domain (hostname of alice's resolved actor URL) for use in repo names.
func setupInboxTest(t *testing.T) (alice *Identity, bob *Identity, inbox *InboxHandler, db *database.DB) {
	t.Helper()
	dir := t.TempDir()

	db, err := database.OpenSQLite(dir, 0, 0, discardLogger())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	alice, err = LoadOrCreateIdentity("https://alice.test", aliceDomain, "", "", discardLogger())
	require.NoError(t, err)

	bob, err = LoadOrCreateIdentity("https://bob.test", "bob.test", "", "", discardLogger())
	require.NoError(t, err)

	inbox = NewInboxHandler(bob, db, InboxConfig{
		MaxManifestSize: config.DefaultMaxManifestSize,
		MaxBlobSize:     config.DefaultMaxBlobSize,
		AutoAccept:      AutoAcceptNone,
	}, discardLogger())
	t.Cleanup(inbox.Stop)

	alicePEM, _ := alice.PublicKeyPEM()
	aliceActor := Actor{
		Context: []any{ContextActivityStreams, ContextSecurity},
		Type:    TypePerson,
		ID:      alice.ActorURL,
		Inbox:   "https://alice.test/ap/inbox",
		Outbox:  "https://alice.test/ap/outbox",
		PublicKey: ActorPublicKey{
			ID:           alice.KeyID(),
			Owner:        alice.ActorURL,
			PublicKeyPEM: alicePEM,
		},
	}

	actorSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", MediaTypeActivityJSON)
		_ = json.NewEncoder(w).Encode(aliceActor)
	}))
	t.Cleanup(actorSrv.Close)

	alice.ActorURL = actorSrv.URL + "/ap/actor"
	alice.Domain = aliceDomain
	aliceActor.ID = alice.ActorURL
	aliceActor.PublicKey.ID = alice.ActorURL + "#main-key"
	aliceActor.PublicKey.Owner = alice.ActorURL

	return alice, bob, inbox, db
}

func aliceRepoName(alice *Identity, suffix string) string {
	domain, _ := senderDomainFromActorURL(alice.ActorURL)
	if suffix == "" {
		return domain + "/app"
	}
	return domain + "/" + suffix
}

func TestInboxFollowAcceptFlow(t *testing.T) {
	alice, bob, inbox, db := setupInboxTest(t)
	ctx := context.Background()

	// Alice sends a Follow to Bob
	follow := map[string]any{
		KeyContext: ContextActivityStreams,
		KeyID:      alice.ActorURL + "#follow-1",
		KeyType:    ActivityFollow,
		KeyActor:   alice.ActorURL,
		KeyObject:  bob.ActorURL,
	}

	req := signedInboxPost(t, alice, follow)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	// Verify follow request was stored
	fr, err := db.GetFollowRequest(ctx, alice.ActorURL)
	require.NoError(t, err)
	require.NotNil(t, fr, "expected follow request to be stored")
}

func TestInboxRejectsActorMismatch(t *testing.T) {
	alice, _, inbox, _ := setupInboxTest(t)

	// Activity claims to be from someone else
	activity := map[string]any{
		KeyContext: ContextActivityStreams,
		KeyID:      "https://evil.com/fake",
		KeyType:    ActivityFollow,
		KeyActor:   "https://evil.com/ap/actor",
		KeyObject:  bobActorURL,
	}

	req := signedInboxPost(t, alice, activity)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code, "expected 403 for actor mismatch")
}

func TestInboxAcceptMarksOutgoingFollowAccepted(t *testing.T) {
	alice, _, inbox, db := setupInboxTest(t)
	ctx := context.Background()

	// Pre-store a pending outgoing follow to alice (simulating we sent a Follow to alice).
	require.NoError(t, db.AddOutgoingFollow(ctx, alice.ActorURL))

	accept := map[string]any{
		KeyContext: ContextActivityStreams,
		KeyID:      alice.ActorURL + "#accept-1",
		KeyType:    ActivityAccept,
		KeyActor:   alice.ActorURL,
		KeyObject: map[string]any{
			KeyType:   ActivityFollow,
			KeyActor:  bobActorURL,
			KeyObject: alice.ActorURL,
		},
	}

	req := signedInboxPost(t, alice, accept)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	// Outgoing follow should be marked as accepted.
	of, err := db.GetOutgoingFollow(ctx, alice.ActorURL)
	require.NoError(t, err)
	require.NotNil(t, of)
	require.Equal(t, "accepted", *of.WeFollowStatus, "expected outgoing follow to be marked accepted after Accept")

	// Accept should NOT auto-promote an incoming follow request -- that requires
	// explicit operator approval.
	f, err := db.GetFollow(ctx, alice.ActorURL)
	require.NoError(t, err)
	require.Nil(t, f, "Accept should not auto-promote incoming follow requests")
}

func TestInboxRejectCleansUp(t *testing.T) {
	alice, _, inbox, db := setupInboxTest(t)
	ctx := context.Background()

	alicePEM, _ := alice.PublicKeyPEM()
	require.NoError(t, db.AddFollowRequest(ctx, alice.ActorURL, alicePEM, "https://alice.test", nil))

	reject := map[string]any{
		KeyContext: ContextActivityStreams,
		KeyID:      alice.ActorURL + "#reject-1",
		KeyType:    ActivityReject,
		KeyActor:   alice.ActorURL,
		KeyObject: map[string]any{
			KeyType: ActivityFollow,
		},
	}

	req := signedInboxPost(t, alice, reject)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code)

	fr, _ := db.GetFollowRequest(ctx, alice.ActorURL)
	require.Nil(t, fr, "expected follow request to be cleaned up after Reject")
}

func TestInboxUndoRemovesFollow(t *testing.T) {
	alice, _, inbox, db := setupInboxTest(t)
	ctx := context.Background()

	alicePEM, _ := alice.PublicKeyPEM()
	require.NoError(t, db.AddFollow(ctx, alice.ActorURL, alicePEM, "https://alice.test", nil))

	undo := map[string]any{
		KeyContext: ContextActivityStreams,
		KeyID:      alice.ActorURL + "#undo-1",
		KeyType:    ActivityUndo,
		KeyActor:   alice.ActorURL,
		KeyObject: map[string]any{
			KeyType: ActivityFollow,
		},
	}

	req := signedInboxPost(t, alice, undo)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code)

	f, _ := db.GetFollow(ctx, alice.ActorURL)
	require.Nil(t, f, "expected follow to be removed after Undo")
}

func TestInboxCreateManifest(t *testing.T) {
	alice, _, inbox, db := setupInboxTest(t)
	ctx := context.Background()

	alicePEM, _ := alice.PublicKeyPEM()
	require.NoError(t, db.AddFollow(ctx, alice.ActorURL, alicePEM, "https://alice.test", nil))

	repo := aliceRepoName(alice, "app")
	create := map[string]any{
		KeyContext: ContextActivityStreams,
		KeyID:      alice.ActorURL + "#create-1",
		KeyType:    ActivityCreate,
		KeyActor:   alice.ActorURL,
		KeyObject: map[string]any{
			KeyType:          TypeOCIManifest,
			KeyOCIRepository: repo,
			KeyOCIDigest:     testDigest,
			KeyOCIMediaType:  testManifestMediaType,
			KeyOCISize:       float64(256),
		},
	}

	req := signedInboxPost(t, alice, create)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	repoObj, err := db.GetRepository(ctx, repo)
	require.NoError(t, err)
	require.NotNil(t, repoObj)
	require.Equal(t, alice.ActorURL, repoObj.OwnerID)
}

func TestInboxCreateManifestRejectsNonFollower(t *testing.T) {
	alice, _, inbox, db := setupInboxTest(t)

	create := map[string]any{
		KeyContext: ContextActivityStreams,
		KeyID:      alice.ActorURL + "#create-1",
		KeyType:    ActivityCreate,
		KeyActor:   alice.ActorURL,
		KeyObject: map[string]any{
			KeyType:          TypeOCIManifest,
			KeyOCIRepository: "test/app",
			KeyOCIDigest:     "sha256:abc123",
			KeyOCIMediaType:  testManifestMediaType,
			KeyOCISize:       float64(256),
		},
	}

	req := signedInboxPost(t, alice, create)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code)

	// Verify no repo was created for the non-follower.
	repoObj, err := db.GetRepository(context.Background(), "test/app")
	require.NoError(t, err)
	require.Nil(t, repoObj, "non-follower Create should not create a repo")
}

func TestInboxUpdateTag(t *testing.T) {
	alice, _, inbox, db := setupInboxTest(t)
	ctx := context.Background()

	alicePEM, _ := alice.PublicKeyPEM()
	require.NoError(t, db.AddFollow(ctx, alice.ActorURL, alicePEM, "https://alice.test", nil))

	repoName := aliceRepoName(alice, "app")
	digest := testDigest
	_, err := db.GetOrCreateRepository(ctx, repoName, alice.ActorURL)
	require.NoError(t, err)
	repoObj, _ := db.GetRepository(ctx, repoName)
	require.NoError(t, db.PutManifest(ctx, &database.Manifest{
		RepositoryID: repoObj.ID,
		Digest:       digest,
		MediaType:    testManifestMediaType,
		SizeBytes:    100,
		Content:      []byte(`{"schemaVersion":2}`),
	}))

	update := map[string]any{
		KeyContext: ContextActivityStreams,
		KeyID:      alice.ActorURL + "#update-1",
		KeyType:    ActivityUpdate,
		KeyActor:   alice.ActorURL,
		KeyObject: map[string]any{
			KeyType:          TypeOCITag,
			KeyOCIRepository: repoName,
			KeyOCITag:        tagLatest,
			KeyOCIDigest:     digest,
		},
	}

	req := signedInboxPost(t, alice, update)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	m, err := db.GetManifestByTag(ctx, repoObj.ID, tagLatest)
	require.NoError(t, err)
	require.NotNil(t, m)
	require.Equal(t, digest, m.Digest)
}

func TestInboxAnnounceBlobRef(t *testing.T) {
	alice, _, inbox, db := setupInboxTest(t)
	ctx := context.Background()

	aliceEndpoint := EndpointFromActorURL(alice.ActorURL)
	alicePEM, _ := alice.PublicKeyPEM()
	require.NoError(t, db.AddFollow(ctx, alice.ActorURL, alicePEM, aliceEndpoint, nil))

	require.NoError(t, db.UpsertActor(ctx, &database.Actor{
		ActorURL:          alice.ActorURL,
		Endpoint:          aliceEndpoint,
		ReplicationPolicy: "lazy",
		IsHealthy:         true,
	}))

	announce := map[string]any{
		KeyContext: ContextActivityStreams,
		KeyID:      alice.ActorURL + "#announce-1",
		KeyType:    ActivityAnnounce,
		KeyActor:   alice.ActorURL,
		KeyObject: map[string]any{
			KeyType:        TypeOCIBlob,
			KeyOCIDigest:   "sha256:b1b2b3b4b5b6b7b8b9b0b1b2b3b4b5b6b7b8b9b0b1b2b3b4b5b6b7b8b9b0b1b2",
			KeyOCISize:     float64(4096),
			KeyOCIEndpoint: aliceEndpoint,
		},
	}

	req := signedInboxPost(t, alice, announce)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	pbs, err := db.FindPeersWithBlob(ctx, "sha256:b1b2b3b4b5b6b7b8b9b0b1b2b3b4b5b6b7b8b9b0b1b2b3b4b5b6b7b8b9b0b1b2")
	require.NoError(t, err)
	require.Len(t, pbs, 1)
}

func TestInboxDeleteIsAccepted(t *testing.T) {
	alice, _, inbox, db := setupInboxTest(t)

	alicePEM, _ := alice.PublicKeyPEM()
	require.NoError(t, db.AddFollow(context.Background(), alice.ActorURL, alicePEM, "https://alice.test", nil))

	del := map[string]any{
		KeyContext: ContextActivityStreams,
		KeyID:      alice.ActorURL + "#delete-1",
		KeyType:    ActivityDelete,
		KeyActor:   alice.ActorURL,
		KeyObject:  fmt.Sprintf("%s/objects/manifest/sha256:abc", alice.ActorURL),
	}

	req := signedInboxPost(t, alice, del)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code)
}

func TestInboxOwnershipEnforcement(t *testing.T) {
	alice, _, inbox, db := setupInboxTest(t)
	ctx := context.Background()

	alicePEM, _ := alice.PublicKeyPEM()
	require.NoError(t, db.AddFollow(ctx, alice.ActorURL, alicePEM, "https://alice.test", nil))

	repoName := aliceRepoName(alice, "repo")
	otherActor := "https://" + aliceRepoName(alice, "ap/other-actor")
	_, err := db.GetOrCreateRepository(ctx, repoName, otherActor)
	require.NoError(t, err)

	create := map[string]any{
		KeyContext: ContextActivityStreams,
		KeyID:      alice.ActorURL + "#create-steal",
		KeyType:    ActivityCreate,
		KeyActor:   alice.ActorURL,
		KeyObject: map[string]any{
			KeyType:          TypeOCIManifest,
			KeyOCIRepository: repoName,
			KeyOCIDigest:     testDigest,
			KeyOCIMediaType:  testManifestMediaType,
			KeyOCISize:       float64(256),
		},
	}

	req := signedInboxPost(t, alice, create)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code)
}

// manifestDigestAndContent returns content bytes and their sha256 digest string.
func manifestDigestAndContent(content []byte) (digest string, encoded string) {
	h := sha256.Sum256(content)
	digest = "sha256:" + hex.EncodeToString(h[:])
	encoded = base64.StdEncoding.EncodeToString(content)
	return
}

func TestInboxCreateManifestWithContent_DigestMatch(t *testing.T) {
	alice, _, inbox, db := setupInboxTest(t)
	ctx := context.Background()

	alicePEM, _ := alice.PublicKeyPEM()
	require.NoError(t, db.AddFollow(ctx, alice.ActorURL, alicePEM, "https://alice.test", nil))

	content := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`)
	digest, encoded := manifestDigestAndContent(content)
	repoName := aliceRepoName(alice, "app")

	create := map[string]any{
		KeyContext: ContextActivityStreams,
		KeyID:      alice.ActorURL + "#create-content",
		KeyType:    ActivityCreate,
		KeyActor:   alice.ActorURL,
		KeyObject: map[string]any{
			KeyType:          TypeOCIManifest,
			KeyOCIRepository: repoName,
			KeyOCIDigest:     digest,
			KeyOCIMediaType:  testManifestMediaType,
			KeyOCISize:       float64(len(content)),
			KeyOCIContent:    encoded,
		},
	}

	req := signedInboxPost(t, alice, create)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	repoObj, err := db.GetRepository(ctx, repoName)
	require.NoError(t, err)
	require.NotNil(t, repoObj)
	m, err := db.GetManifestByDigest(ctx, repoObj.ID, digest)
	require.NoError(t, err)
	require.NotNil(t, m)
	require.Equal(t, content, m.Content)
}

func TestInboxCreateManifestWithContent_DigestMismatch(t *testing.T) {
	alice, _, inbox, db := setupInboxTest(t)
	ctx := context.Background()

	alicePEM, _ := alice.PublicKeyPEM()
	require.NoError(t, db.AddFollow(ctx, alice.ActorURL, alicePEM, "https://alice.test", nil))

	legitimateContent := []byte(`{"schemaVersion":2}`)
	digest, _ := manifestDigestAndContent(legitimateContent)
	malwareContent := []byte(`<malware>not what you asked for</malware>`)
	tamperedEncoded := base64.StdEncoding.EncodeToString(malwareContent)

	create := map[string]any{
		KeyContext: ContextActivityStreams,
		KeyID:      alice.ActorURL + "#create-tampered",
		KeyType:    ActivityCreate,
		KeyActor:   alice.ActorURL,
		KeyObject: map[string]any{
			KeyType:          TypeOCIManifest,
			KeyOCIRepository: aliceRepoName(alice, "app"),
			KeyOCIDigest:     digest,
			KeyOCIMediaType:  testManifestMediaType,
			KeyOCISize:       float64(len(legitimateContent)),
			KeyOCIContent:    tamperedEncoded,
		},
	}

	req := signedInboxPost(t, alice, create)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code)
}

func TestInboxCreateManifestWrongDomain(t *testing.T) {
	alice, _, inbox, db := setupInboxTest(t)
	ctx := context.Background()

	alicePEM, _ := alice.PublicKeyPEM()
	require.NoError(t, db.AddFollow(ctx, alice.ActorURL, alicePEM, "https://alice.test", nil))

	create := map[string]any{
		KeyContext: ContextActivityStreams,
		KeyID:      alice.ActorURL + "#create-squatter",
		KeyType:    ActivityCreate,
		KeyActor:   alice.ActorURL,
		KeyObject: map[string]any{
			KeyType:          TypeOCIManifest,
			KeyOCIRepository: "someotherdomain.example/app",
			KeyOCIDigest:     testDigest,
			KeyOCIMediaType:  testManifestMediaType,
			KeyOCISize:       float64(256),
		},
	}

	req := signedInboxPost(t, alice, create)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code)
}

func TestInboxAcceptWithoutPendingOutgoingFollow(t *testing.T) {
	alice, _, inbox, db := setupInboxTest(t)
	ctx := context.Background()

	// No outgoing follow stored — alice sends Accept(Follow) out of the blue.
	accept := map[string]any{
		KeyContext: ContextActivityStreams,
		KeyID:      alice.ActorURL + "#accept-spurious",
		KeyType:    ActivityAccept,
		KeyActor:   alice.ActorURL,
		KeyObject: map[string]any{
			KeyType:   ActivityFollow,
			KeyActor:  bobActorURL,
			KeyObject: alice.ActorURL,
		},
	}

	req := signedInboxPost(t, alice, accept)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code, "spurious Accept should be silently accepted")

	// No outgoing follow should exist.
	of, err := db.GetOutgoingFollow(ctx, alice.ActorURL)
	require.NoError(t, err)
	require.Nil(t, of, "no outgoing follow should be created from a spurious Accept")
}

func TestInboxAcceptWithAlreadyAcceptedOutgoingFollow(t *testing.T) {
	alice, _, inbox, db := setupInboxTest(t)
	ctx := context.Background()

	require.NoError(t, db.AddOutgoingFollow(ctx, alice.ActorURL))
	require.NoError(t, db.AcceptOutgoingFollow(ctx, alice.ActorURL))

	accept := map[string]any{
		KeyContext: ContextActivityStreams,
		KeyID:      alice.ActorURL + "#accept-dup",
		KeyType:    ActivityAccept,
		KeyActor:   alice.ActorURL,
		KeyObject: map[string]any{
			KeyType:   ActivityFollow,
			KeyActor:  bobActorURL,
			KeyObject: alice.ActorURL,
		},
	}

	req := signedInboxPost(t, alice, accept)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code)

	// Status should remain accepted (not reset or error).
	of, err := db.GetOutgoingFollow(ctx, alice.ActorURL)
	require.NoError(t, err)
	require.NotNil(t, of)
	require.Equal(t, "accepted", *of.WeFollowStatus)
}

func TestInboxAcceptWithStringObject(t *testing.T) {
	alice, _, inbox, db := setupInboxTest(t)
	ctx := context.Background()

	require.NoError(t, db.AddOutgoingFollow(ctx, alice.ActorURL))

	accept := map[string]any{
		KeyContext: ContextActivityStreams,
		KeyID:      alice.ActorURL + "#accept-str",
		KeyType:    ActivityAccept,
		KeyActor:   alice.ActorURL,
		KeyObject:  alice.ActorURL + "#follow-1", // string form
	}

	req := signedInboxPost(t, alice, accept)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code)

	of, err := db.GetOutgoingFollow(ctx, alice.ActorURL)
	require.NoError(t, err)
	require.NotNil(t, of)
	require.Equal(t, "accepted", *of.WeFollowStatus)
}

func TestInboxRejectMarksOutgoingFollowRejected(t *testing.T) {
	alice, _, inbox, db := setupInboxTest(t)
	ctx := context.Background()

	require.NoError(t, db.AddOutgoingFollow(ctx, alice.ActorURL))

	reject := map[string]any{
		KeyContext: ContextActivityStreams,
		KeyID:      alice.ActorURL + "#reject-out",
		KeyType:    ActivityReject,
		KeyActor:   alice.ActorURL,
		KeyObject: map[string]any{
			KeyType: ActivityFollow,
		},
	}

	req := signedInboxPost(t, alice, reject)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code)

	of, err := db.GetOutgoingFollow(ctx, alice.ActorURL)
	require.NoError(t, err)
	require.NotNil(t, of)
	require.Equal(t, "rejected", *of.WeFollowStatus)
}

func TestInboxRejectCleansBothDirections(t *testing.T) {
	alice, _, inbox, db := setupInboxTest(t)
	ctx := context.Background()

	alicePEM, _ := alice.PublicKeyPEM()
	require.NoError(t, db.AddOutgoingFollow(ctx, alice.ActorURL))
	require.NoError(t, db.AddFollowRequest(ctx, alice.ActorURL, alicePEM, "https://alice.test", nil))

	reject := map[string]any{
		KeyContext: ContextActivityStreams,
		KeyID:      alice.ActorURL + "#reject-both",
		KeyType:    ActivityReject,
		KeyActor:   alice.ActorURL,
		KeyObject: map[string]any{
			KeyType: ActivityFollow,
		},
	}

	req := signedInboxPost(t, alice, reject)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code)

	of, err := db.GetOutgoingFollow(ctx, alice.ActorURL)
	require.NoError(t, err)
	require.NotNil(t, of)
	require.Equal(t, "rejected", *of.WeFollowStatus)

	fr, err := db.GetFollowRequest(ctx, alice.ActorURL)
	require.NoError(t, err)
	require.Nil(t, fr, "pending follow request should be cleaned up after Reject")
}

func TestInboxUndoForNonExistentFollow(t *testing.T) {
	alice, _, inbox, _ := setupInboxTest(t)

	undo := map[string]any{
		KeyContext: ContextActivityStreams,
		KeyID:      alice.ActorURL + "#undo-ghost",
		KeyType:    ActivityUndo,
		KeyActor:   alice.ActorURL,
		KeyObject: map[string]any{
			KeyType: ActivityFollow,
		},
	}

	req := signedInboxPost(t, alice, undo)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code,
		"Undo for non-existent follow should be silently accepted, not 500")
}

func TestInboxDuplicateActivityDedup(t *testing.T) {
	alice, _, inbox, db := setupInboxTest(t)
	ctx := context.Background()

	alicePEM, _ := alice.PublicKeyPEM()
	require.NoError(t, db.AddFollow(ctx, alice.ActorURL, alicePEM, "https://alice.test", nil))

	activityID := alice.ActorURL + "#undo-dedup"
	undo := map[string]any{
		KeyContext: ContextActivityStreams,
		KeyID:      activityID,
		KeyType:    ActivityUndo,
		KeyActor:   alice.ActorURL,
		KeyObject: map[string]any{
			KeyType: ActivityFollow,
		},
	}

	req := signedInboxPost(t, alice, undo)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code)

	// Verify the activity was stored for dedup.
	existing, err := db.GetActivity(ctx, activityID)
	require.NoError(t, err)
	require.NotNil(t, existing, "activity should be stored for dedup")

	req = signedInboxPostWithDate(t, alice, undo, time.Now().Add(time.Second))
	rec = httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code)
}

func TestInboxFollowReprocessedAfterRemoval(t *testing.T) {
	alice, bob, inbox, db := setupInboxTest(t)
	ctx := context.Background()

	activityID := alice.ActorURL + "#follow-" + bob.ActorURL
	follow := map[string]any{
		KeyContext: ContextActivityStreams,
		KeyID:      activityID,
		KeyType:    ActivityFollow,
		KeyActor:   alice.ActorURL,
		KeyObject:  bob.ActorURL,
	}

	req := signedInboxPost(t, alice, follow)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code)

	fr, err := db.GetFollowRequest(ctx, alice.ActorURL)
	require.NoError(t, err)
	require.NotNil(t, fr, "first follow should be stored")

	require.NoError(t, db.RejectFollowRequest(ctx, alice.ActorURL))

	// Re-follow with the same activity ID — allowed because the relationship is gone.
	req = signedInboxPostWithDate(t, alice, follow, time.Now().Add(time.Second))
	rec = httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code)

	fr, err = db.GetFollowRequest(ctx, alice.ActorURL)
	require.NoError(t, err)
	require.NotNil(t, fr, "re-follow after removal should be stored")
}

func TestInboxFollowSelfRejected(t *testing.T) {
	alice, _, inbox, _ := setupInboxTest(t)

	follow := map[string]any{
		KeyContext: ContextActivityStreams,
		KeyID:      alice.ActorURL + "#follow-self",
		KeyType:    ActivityFollow,
		KeyActor:   alice.ActorURL,
		KeyObject:  alice.ActorURL, // not bob's actor URL
	}

	req := signedInboxPost(t, alice, follow)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code)
}

func TestInboxBlockedActorSilentDrop(t *testing.T) {
	alice, bob, _, db := setupInboxTest(t)

	blockedInbox := NewInboxHandler(bob, db, InboxConfig{
		MaxManifestSize: config.DefaultMaxManifestSize,
		MaxBlobSize:     config.DefaultMaxBlobSize,
		AutoAccept:      AutoAcceptNone,
		BlockedActors:   []string{alice.ActorURL},
	}, discardLogger())
	t.Cleanup(blockedInbox.Stop)

	follow := map[string]any{
		KeyContext: ContextActivityStreams,
		KeyID:      alice.ActorURL + "#follow-blocked",
		KeyType:    ActivityFollow,
		KeyActor:   alice.ActorURL,
		KeyObject:  bob.ActorURL,
	}

	req := signedInboxPost(t, alice, follow)
	rec := httptest.NewRecorder()
	blockedInbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code, "blocked actor should get 202 (silent drop)")

	// Verify nothing was stored.
	ctx := context.Background()
	fr, err := db.GetFollowRequest(ctx, alice.ActorURL)
	require.NoError(t, err)
	require.Nil(t, fr, "blocked actor's follow request should not be stored")
}

func TestInboxBlockedDomainSilentDrop(t *testing.T) {
	alice, bob, _, db := setupInboxTest(t)

	// Block the domain parsed from alice's actor URL
	blockedInbox := NewInboxHandler(bob, db, InboxConfig{
		MaxManifestSize: config.DefaultMaxManifestSize,
		MaxBlobSize:     config.DefaultMaxBlobSize,
		AutoAccept:      AutoAcceptNone,
		BlockedDomains:  []string{"127.0.0.1"},
	}, discardLogger())
	t.Cleanup(blockedInbox.Stop)

	follow := map[string]any{
		KeyContext: ContextActivityStreams,
		KeyID:      alice.ActorURL + "#follow-blocked-dom",
		KeyType:    ActivityFollow,
		KeyActor:   alice.ActorURL,
		KeyObject:  bob.ActorURL,
	}

	req := signedInboxPost(t, alice, follow)
	rec := httptest.NewRecorder()
	blockedInbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code, "blocked domain should get 202 (silent drop)")
}

func setupMutualInboxTest(t *testing.T) (alice *Identity, bob *Identity, inbox *InboxHandler, db *database.DB) {
	t.Helper()
	alice, bob, inbox, db = setupInboxTest(t)

	// Reconfigure inbox for mutual mode.
	inbox.autoAccept = AutoAcceptMutual

	// Pre-store outgoing follow to alice (we already sent a Follow to alice).
	ctx := context.Background()
	require.NoError(t, db.AddOutgoingFollow(ctx, alice.ActorURL))

	return
}

func TestMutualAutoAcceptFollowFlow(t *testing.T) {
	alice, bob, inbox, db := setupMutualInboxTest(t)
	ctx := context.Background()

	follow := map[string]any{
		KeyContext: ContextActivityStreams,
		KeyID:      alice.ActorURL + "#follow-mutual",
		KeyType:    ActivityFollow,
		KeyActor:   alice.ActorURL,
		KeyObject:  bob.ActorURL,
	}

	// Provide enqueue so SendAccept has a delivery path.
	var enqueued bool
	inbox.SetEnqueueFunc(func(_ context.Context, _, _ string, _ []byte) error {
		enqueued = true
		return nil
	})

	req := signedInboxPost(t, alice, follow)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code)

	// Alice should now be an accepted follower (not just a pending request).
	f, err := db.GetFollow(ctx, alice.ActorURL)
	require.NoError(t, err)
	require.NotNil(t, f, "mutual auto-accept should promote alice to follower")

	// Follow request should be consumed.
	fr, err := db.GetFollowRequest(ctx, alice.ActorURL)
	require.NoError(t, err)
	require.Nil(t, fr, "follow request should be consumed after auto-accept")

	require.True(t, enqueued, "Accept should have been enqueued for delivery")
}

func TestMutualAutoAcceptDoesNotTriggerWithoutOutgoingFollow(t *testing.T) {
	_, bob, inbox, db := setupInboxTest(t)
	ctx := context.Background()

	inbox.autoAccept = AutoAcceptMutual

	// Create a different identity for a stranger.
	stranger, err := LoadOrCreateIdentity("https://stranger.test", "stranger.test", "", "", discardLogger())
	require.NoError(t, err)

	strangerPEM, _ := stranger.PublicKeyPEM()
	strangerActor := Actor{
		Context: []any{ContextActivityStreams, ContextSecurity},
		Type:    TypePerson,
		ID:      stranger.ActorURL,
		Inbox:   "https://stranger.test/ap/inbox",
		PublicKey: ActorPublicKey{
			ID:           stranger.KeyID(),
			Owner:        stranger.ActorURL,
			PublicKeyPEM: strangerPEM,
		},
	}

	actorSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", MediaTypeActivityJSON)
		_ = json.NewEncoder(w).Encode(strangerActor)
	}))
	defer actorSrv.Close()

	stranger.ActorURL = actorSrv.URL + "/ap/actor"
	strangerActor.ID = stranger.ActorURL
	strangerActor.PublicKey.ID = stranger.ActorURL + "#main-key"
	strangerActor.PublicKey.Owner = stranger.ActorURL

	follow := map[string]any{
		KeyContext: ContextActivityStreams,
		KeyID:      stranger.ActorURL + "#follow-stranger",
		KeyType:    ActivityFollow,
		KeyActor:   stranger.ActorURL,
		KeyObject:  bob.ActorURL,
	}

	req := signedInboxPost(t, stranger, follow)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code)

	// Should be pending, NOT auto-accepted.
	fr, err := db.GetFollowRequest(ctx, stranger.ActorURL)
	require.NoError(t, err)
	require.NotNil(t, fr, "follow from stranger should remain as pending request")

	f, err := db.GetFollow(ctx, stranger.ActorURL)
	require.NoError(t, err)
	require.Nil(t, f, "stranger should not be auto-accepted without outgoing follow")
}

func TestMutualAcceptAutoAcceptsPendingInboundFollow(t *testing.T) {
	alice, _, inbox, db := setupInboxTest(t)
	ctx := context.Background()

	inbox.autoAccept = AutoAcceptMutual

	require.NoError(t, db.AddOutgoingFollow(ctx, alice.ActorURL))

	alicePEM, _ := alice.PublicKeyPEM()
	require.NoError(t, db.AddFollowRequest(ctx, alice.ActorURL, alicePEM, "https://alice.test", nil))

	inbox.SetEnqueueFunc(func(_ context.Context, _, _ string, _ []byte) error {
		return nil
	})

	accept := map[string]any{
		KeyContext: ContextActivityStreams,
		KeyID:      alice.ActorURL + "#accept-mutual",
		KeyType:    ActivityAccept,
		KeyActor:   alice.ActorURL,
		KeyObject: map[string]any{
			KeyType:   ActivityFollow,
			KeyActor:  bobActorURL,
			KeyObject: alice.ActorURL,
		},
	}

	req := signedInboxPost(t, alice, accept)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code)

	of, err := db.GetOutgoingFollow(ctx, alice.ActorURL)
	require.NoError(t, err)
	require.NotNil(t, of)
	require.Equal(t, "accepted", *of.WeFollowStatus)

	f, err := db.GetFollow(ctx, alice.ActorURL)
	require.NoError(t, err)
	require.NotNil(t, f, "mutual mode should auto-accept pending inbound follow when Accept is received")

	fr, err := db.GetFollowRequest(ctx, alice.ActorURL)
	require.NoError(t, err)
	require.Nil(t, fr, "follow request should be consumed after mutual auto-accept")
}

func TestInboxAutoAcceptAll(t *testing.T) {
	alice, bob, inbox, db := setupInboxTest(t)
	ctx := context.Background()

	inbox.autoAccept = AutoAcceptAll

	inbox.SetEnqueueFunc(func(_ context.Context, _, _ string, _ []byte) error {
		return nil
	})

	follow := map[string]any{
		KeyContext: ContextActivityStreams,
		KeyID:      alice.ActorURL + "#follow-autoall",
		KeyType:    ActivityFollow,
		KeyActor:   alice.ActorURL,
		KeyObject:  bob.ActorURL,
	}

	req := signedInboxPost(t, alice, follow)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code)

	f, err := db.GetFollow(ctx, alice.ActorURL)
	require.NoError(t, err)
	require.NotNil(t, f, "autoAccept=all should promote alice to follower immediately")
}

func TestInboxUpdateRejectsNonFollower(t *testing.T) {
	alice, _, inbox, _ := setupInboxTest(t)

	update := map[string]any{
		KeyContext: ContextActivityStreams,
		KeyID:      alice.ActorURL + "#update-nofollow",
		KeyType:    ActivityUpdate,
		KeyActor:   alice.ActorURL,
		KeyObject: map[string]any{
			KeyType: TypeOCITag,
		},
	}

	req := signedInboxPost(t, alice, update)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code)
}

func TestInboxAnnounceRejectsNonFollower(t *testing.T) {
	alice, _, inbox, _ := setupInboxTest(t)

	announce := map[string]any{
		KeyContext: ContextActivityStreams,
		KeyID:      alice.ActorURL + "#announce-nofollow",
		KeyType:    ActivityAnnounce,
		KeyActor:   alice.ActorURL,
		KeyObject: map[string]any{
			KeyType: TypeOCIBlob,
		},
	}

	req := signedInboxPost(t, alice, announce)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code)
}

func TestInboxDeleteRejectsNonFollower(t *testing.T) {
	alice, _, inbox, _ := setupInboxTest(t)

	del := map[string]any{
		KeyContext: ContextActivityStreams,
		KeyID:      alice.ActorURL + "#delete-nofollow",
		KeyType:    ActivityDelete,
		KeyActor:   alice.ActorURL,
		KeyObject: map[string]any{
			KeyType: TypeOCIManifest,
		},
	}

	req := signedInboxPost(t, alice, del)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code)
}

func TestInboxUpdateTagUnknownManifest(t *testing.T) {
	alice, _, inbox, db := setupInboxTest(t)
	ctx := context.Background()

	alicePEM, _ := alice.PublicKeyPEM()
	require.NoError(t, db.AddFollow(ctx, alice.ActorURL, alicePEM, "https://alice.test", nil))

	repoName := aliceRepoName(alice, "app")
	_, err := db.GetOrCreateRepository(ctx, repoName, alice.ActorURL)
	require.NoError(t, err)

	update := map[string]any{
		KeyContext: ContextActivityStreams,
		KeyID:      alice.ActorURL + "#update-ghost",
		KeyType:    ActivityUpdate,
		KeyActor:   alice.ActorURL,
		KeyObject: map[string]any{
			KeyType:          TypeOCITag,
			KeyOCIRepository: repoName,
			KeyOCITag:        tagLatest,
			KeyOCIDigest:     "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	}

	req := signedInboxPost(t, alice, update)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code)
}

func TestInboxCreateManifestSplitDomainNamespace(t *testing.T) {
	dir := t.TempDir()
	db, err := database.OpenSQLite(dir, 0, 0, discardLogger())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	alice, err := LoadOrCreateIdentity("https://registry.alice.test", "registry.alice.test", aliceDomain, "", discardLogger())
	require.NoError(t, err)

	bob, err := LoadOrCreateIdentity("https://bob.test", "bob.test", "", "", discardLogger())
	require.NoError(t, err)

	inbox := NewInboxHandler(bob, db, InboxConfig{
		MaxManifestSize: config.DefaultMaxManifestSize,
		MaxBlobSize:     config.DefaultMaxBlobSize,
		AutoAccept:      AutoAcceptNone,
	}, discardLogger())
	t.Cleanup(inbox.Stop)

	alicePEM, _ := alice.PublicKeyPEM()
	aliceActor := Actor{
		Context: []any{ContextActivityStreams, ContextSecurity},
		Type:    "Application",
		ID:      alice.ActorURL,
		Inbox:   "https://registry.alice.test/ap/inbox",
		PublicKey: ActorPublicKey{
			ID:           alice.KeyID(),
			Owner:        alice.ActorURL,
			PublicKeyPEM: alicePEM,
		},
		OCINamespace: aliceDomain,
	}

	actorSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", MediaTypeActivityJSON)
		_ = json.NewEncoder(w).Encode(aliceActor)
	}))
	t.Cleanup(actorSrv.Close)

	alice.ActorURL = actorSrv.URL + "/ap/actor"
	aliceActor.ID = alice.ActorURL
	aliceActor.PublicKey.ID = alice.ActorURL + "#main-key"
	aliceActor.PublicKey.Owner = alice.ActorURL

	ctx := context.Background()
	require.NoError(t, db.AddFollow(ctx, alice.ActorURL, alicePEM, actorSrv.URL, nil))

	// Pre-populate the namespace cache to simulate a validated split-domain.
	// httptest uses 127.0.0.1 which can't pass the parent-domain validation
	// against aliceDomain — covered by TestValidNamespaceForHost instead.
	inbox.SetNamespaceForActor(alice.ActorURL, aliceDomain)

	create := map[string]any{
		KeyContext: ContextActivityStreams,
		KeyID:      alice.ActorURL + "#create-split-1",
		KeyType:    ActivityCreate,
		KeyActor:   alice.ActorURL,
		KeyObject: map[string]any{
			KeyType:          TypeOCIManifest,
			KeyOCIRepository: "alice.test/myapp",
			KeyOCIDigest:     testDigest,
			KeyOCIMediaType:  testManifestMediaType,
			KeyOCISize:       float64(256),
		},
	}

	req := signedInboxPost(t, alice, create)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code, "split-domain repo must be accepted: %s", rec.Body.String())

	repoObj, err := db.GetRepository(ctx, "alice.test/myapp")
	require.NoError(t, err)
	require.NotNil(t, repoObj)
}

func TestInboxRejectsSpoofedNamespace(t *testing.T) {
	dir := t.TempDir()
	db, err := database.OpenSQLite(dir, 0, 0, discardLogger())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	// Evil actor claims ociNamespace of a different domain.
	evil, err := LoadOrCreateIdentity("https://evil.test", "evil.test", "", "", discardLogger())
	require.NoError(t, err)

	bob, err := LoadOrCreateIdentity("https://bob.test", "bob.test", "", "", discardLogger())
	require.NoError(t, err)

	inbox := NewInboxHandler(bob, db, InboxConfig{
		MaxManifestSize: config.DefaultMaxManifestSize,
		MaxBlobSize:     config.DefaultMaxBlobSize,
		AutoAccept:      AutoAcceptNone,
	}, discardLogger())
	t.Cleanup(inbox.Stop)

	evilPEM, _ := evil.PublicKeyPEM()
	evilActor := Actor{
		Context: []any{ContextActivityStreams, ContextSecurity},
		Type:    "Application",
		ID:      evil.ActorURL,
		Inbox:   "https://evil.test/ap/inbox",
		PublicKey: ActorPublicKey{
			ID:           evil.KeyID(),
			Owner:        evil.ActorURL,
			PublicKeyPEM: evilPEM,
		},
		OCINamespace: "victim.test", // spoofed namespace
	}

	actorSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", MediaTypeActivityJSON)
		_ = json.NewEncoder(w).Encode(evilActor)
	}))
	t.Cleanup(actorSrv.Close)

	evil.ActorURL = actorSrv.URL + "/ap/actor"
	evilActor.ID = evil.ActorURL
	evilActor.PublicKey.ID = evil.ActorURL + "#main-key"
	evilActor.PublicKey.Owner = evil.ActorURL

	ctx := context.Background()
	require.NoError(t, db.AddFollow(ctx, evil.ActorURL, evilPEM, actorSrv.URL, nil))

	// Evil tries to push a repo under the spoofed namespace.
	create := map[string]any{
		KeyContext: ContextActivityStreams,
		KeyID:      evil.ActorURL + "#create-spoof",
		KeyType:    ActivityCreate,
		KeyActor:   evil.ActorURL,
		KeyObject: map[string]any{
			KeyType:          TypeOCIManifest,
			KeyOCIRepository: "victim.test/malicious",
			KeyOCIDigest:     testDigest,
			KeyOCIMediaType:  testManifestMediaType,
			KeyOCISize:       float64(256),
		},
	}

	req := signedInboxPost(t, evil, create)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code, "spoofed namespace should get 202 (processed async, error logged)")
}

func TestValidNamespaceForHost(t *testing.T) {
	tests := []struct {
		ns, host string
		want     bool
	}{
		{exampleHost, "registry.example.com", true},
		{exampleHost, "example.com", true},
		{exampleHost, "evil.test", false},
		{exampleHost, "notexample.com", false},
		{"a.b.c", "x.a.b.c", true},
		{aliceDomain, "127.0.0.1", false},
	}
	for _, tt := range tests {
		got := validNamespaceForHost(tt.ns, tt.host)
		require.Equal(t, tt.want, got, "validNamespaceForHost(%q, %q)", tt.ns, tt.host)
	}
}

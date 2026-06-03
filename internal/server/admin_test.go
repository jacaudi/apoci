package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/activitypub"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/federation"
)

const testToken = "test-token"

type mockAPFederator struct {
	resolveFollowTargetFn func(ctx context.Context, input string) (string, error)
	fetchActorFn          func(ctx context.Context, actorURL string) (*activitypub.Actor, error)
	sendAcceptFn          func(ctx context.Context, followerActorURL string) error
	sendRejectFn          func(ctx context.Context, followerActorURL string) error
	sendFollowFn          func(ctx context.Context, targetActorURL string) (string, error)
}

func (m *mockAPFederator) ResolveFollowTarget(ctx context.Context, input string) (string, error) {
	if m.resolveFollowTargetFn != nil {
		return m.resolveFollowTargetFn(ctx, input)
	}
	return input, nil
}

func (m *mockAPFederator) FetchActor(ctx context.Context, actorURL string) (*activitypub.Actor, error) {
	if m.fetchActorFn != nil {
		return m.fetchActorFn(ctx, actorURL)
	}
	return nil, errors.New("fetchActor not configured")
}

func (m *mockAPFederator) SendAccept(ctx context.Context, followerActorURL string) error {
	if m.sendAcceptFn != nil {
		return m.sendAcceptFn(ctx, followerActorURL)
	}
	return errors.New("sendAccept not configured")
}

func (m *mockAPFederator) SendReject(ctx context.Context, followerActorURL string) error {
	if m.sendRejectFn != nil {
		return m.sendRejectFn(ctx, followerActorURL)
	}
	return errors.New("sendReject not configured")
}

func (m *mockAPFederator) SendUndo(_ context.Context, _ string) error {
	return nil
}

func (m *mockAPFederator) SendFollow(ctx context.Context, targetActorURL string) (string, error) {
	if m.sendFollowFn != nil {
		return m.sendFollowFn(ctx, targetActorURL)
	}
	return targetActorURL, nil
}

func testServerWithMock(t *testing.T, fed federation.Federator) *Server {
	t.Helper()
	s := testServer(t)
	s.fedSvc = &federation.Service{
		Fed:      fed,
		DB:       s.db,
		ActorURL: s.identity.ActorURL,
		Logger:   s.logger,
	}
	return s
}

func adminActor(actorURL, inboxURL string) *activitypub.Actor { //nolint:unparam // test helper, param aids readability
	return &activitypub.Actor{
		ID:    actorURL,
		Inbox: inboxURL,
		PublicKey: activitypub.ActorPublicKey{
			ID:           actorURL + "#main-key",
			Owner:        actorURL,
			PublicKeyPEM: "-----BEGIN PUBLIC KEY-----\nfake\n-----END PUBLIC KEY-----",
		},
	}
}

func TestAdminGetIdentityFields(t *testing.T) {
	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/api/admin/identity", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "application/json", resp.Header.Get("Content-Type"))

	var info map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&info))
	require.Equal(t, "test-node", info["name"])
	require.Equal(t, "test.example.com", info["domain"])
	require.Equal(t, "test.example.com", info["accountDomain"])
	require.Equal(t, "https://test.example.com", info["endpoint"])
	require.NotEmpty(t, info["actorURL"])
	require.NotEmpty(t, info["keyID"])
	require.Contains(t, info["publicKey"], "-----BEGIN PUBLIC KEY-----")
}

func TestAdminPauseNormalizesDomain(t *testing.T) {
	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	post := func(path, body string) int {
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-token")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode
	}

	// Mixed-case + whitespace is normalized to the bare lowercase host that
	// isBlocked compares against.
	require.Equal(t, http.StatusOK, post("/api/admin/peers/pause", `{"domain":"  Peer.Example.com  "}`))

	// A scheme/path is rejected rather than silently stored as a non-matching key.
	require.Equal(t, http.StatusBadRequest, post("/api/admin/peers/pause", `{"domain":"https://evil.example.com"}`))

	req, _ := http.NewRequest("GET", srv.URL+"/api/admin/peers/blocked", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var blocked map[string][]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&blocked))
	require.Equal(t, []string{"peer.example.com"}, blocked["domains"])
}

func TestAdminListFollowsEmpty(t *testing.T) {
	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+testFollowsAPI, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var follows []any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&follows))
	require.Empty(t, follows)
}

func TestAdminListFollowsWithData(t *testing.T) {
	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	ctx := context.Background()

	require.NoError(t, s.db.AddFollow(ctx, "https://alice.example.com/ap/actor", "pubkey-alice", "https://alice.example.com", nil))
	require.NoError(t, s.db.AddFollow(ctx, "https://bob.example.com/ap/actor", "pubkey-bob", "https://bob.example.com", nil))

	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+testFollowsAPI, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var follows []struct {
		ActorURL string `json:"actor_url"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&follows))
	require.Len(t, follows, 2)

	seen := make(map[string]bool)
	for _, f := range follows {
		seen[f.ActorURL] = true
	}
	require.True(t, seen["https://alice.example.com/ap/actor"])
	require.True(t, seen["https://bob.example.com/ap/actor"])
}

func TestAdminListFollowsInternalError(t *testing.T) {
	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	_ = s.db.Close() // force DB error

	req, _ := http.NewRequest("GET", srv.URL+testFollowsAPI, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

func TestAdminListPendingEmpty(t *testing.T) {
	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/api/admin/follows/pending", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var requests []any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&requests))
	require.Empty(t, requests)
}

func TestAdminListPendingWithData(t *testing.T) {
	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	ctx := context.Background()

	require.NoError(t, s.db.AddFollowRequest(ctx, "https://carol.example.com/ap/actor", "pubkey-carol", "https://carol.example.com", nil))

	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/api/admin/follows/pending", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var requests []struct {
		ActorURL string `json:"ActorURL"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&requests))
	require.Len(t, requests, 1)
	require.Equal(t, "https://carol.example.com/ap/actor", requests[0].ActorURL)
}

func TestAdminAddFollowMissingTarget(t *testing.T) {
	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	for _, body := range []string{`{}`, `{"target":""}`, `not-json`} {
		req, _ := http.NewRequest("POST", srv.URL+testFollowsAPI, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusBadRequest, resp.StatusCode, "body: %s", body)
	}
}

func TestAdminAddFollowResolveError(t *testing.T) {
	fed := &mockAPFederator{
		resolveFollowTargetFn: func(_ context.Context, _ string) (string, error) {
			return "", errors.New("resolve failed")
		},
	}
	s := testServerWithMock(t, fed)
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+testFollowsAPI, strings.NewReader(`{"target":"bad-target"}`))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.Equal(t, http.StatusBadGateway, resp.StatusCode)
}

func TestAdminAddFollowFetchActorError(t *testing.T) {
	const actorURL = "https://peer.example.com/ap/actor"
	fed := &mockAPFederator{
		fetchActorFn: func(_ context.Context, _ string) (*activitypub.Actor, error) {
			return nil, errors.New("unreachable")
		},
	}
	s := testServerWithMock(t, fed)
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	body := `{"target":"` + actorURL + `"}`
	req, _ := http.NewRequest("POST", srv.URL+testFollowsAPI, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.Equal(t, http.StatusBadGateway, resp.StatusCode)
}

func TestAdminAddFollowDeliveryError(t *testing.T) {
	const actorURL = "https://peer.example.com/ap/actor"
	const inboxURL = "https://peer.example.com/ap/inbox"
	fed := &mockAPFederator{
		fetchActorFn: func(_ context.Context, _ string) (*activitypub.Actor, error) {
			return adminActor(actorURL, inboxURL), nil
		},
		sendFollowFn: func(_ context.Context, _ string) (string, error) {
			return "", errors.New("delivery failed")
		},
	}
	s := testServerWithMock(t, fed)
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	body := `{"target":"` + actorURL + `"}`
	req, _ := http.NewRequest("POST", srv.URL+testFollowsAPI, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.Equal(t, http.StatusBadGateway, resp.StatusCode)
}

func TestAdminAddFollowSuccess(t *testing.T) {
	const actorURL = "https://peer.example.com/ap/actor"
	const inboxURL = "https://peer.example.com/ap/inbox"

	var sendFollowTarget string
	fed := &mockAPFederator{
		fetchActorFn: func(_ context.Context, _ string) (*activitypub.Actor, error) {
			return adminActor(actorURL, inboxURL), nil
		},
		sendFollowFn: func(_ context.Context, target string) (string, error) {
			sendFollowTarget = target
			return target, nil
		},
	}
	s := testServerWithMock(t, fed)
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	body := `{"target":"` + actorURL + `"}`
	req, _ := http.NewRequest("POST", srv.URL+testFollowsAPI, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	require.Equal(t, actorURL, result["followed"])
	require.Equal(t, actorURL, sendFollowTarget, "SendFollow should be called with the canonical actor ID")

	ctx := context.Background()
	of, err := s.db.GetOutgoingFollow(ctx, actorURL)
	require.NoError(t, err)
	require.NotNil(t, of, "outgoing follow should be persisted")
	require.NotNil(t, of.WeFollowStatus)
	require.Equal(t, "pending", *of.WeFollowStatus)
}

func TestAdminAcceptFollowMissingTarget(t *testing.T) {
	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/api/admin/follows/accept", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestAdminAcceptFollowNoPendingRequest(t *testing.T) {
	const actorURL = "https://peer.example.com/ap/actor"
	fed := &mockAPFederator{}
	s := testServerWithMock(t, fed)
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	body := `{"target":"` + actorURL + `"}`
	req, _ := http.NewRequest("POST", srv.URL+"/api/admin/follows/accept", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

func TestAdminAcceptFollowSendAcceptBestEffort(t *testing.T) {
	const actorURL = "https://peer.example.com/ap/actor"
	const inboxURL = "https://peer.example.com/ap/inbox"

	ctx := context.Background()

	fed := &mockAPFederator{
		sendAcceptFn: func(_ context.Context, _ string) error {
			return errors.New("peer unreachable")
		},
	}
	s := testServerWithMock(t, fed)
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	require.NoError(t, s.db.AddFollowRequest(ctx, actorURL, "pubkey-peer", inboxURL, nil))

	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	body := `{"target":"` + actorURL + `"}`
	req, _ := http.NewRequest("POST", srv.URL+"/api/admin/follows/accept", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	require.Equal(t, actorURL, result["accepted"])

	f, err := s.db.GetFollow(ctx, actorURL)
	require.NoError(t, err)
	require.NotNil(t, f)
}

func TestAdminAcceptFollowSuccess(t *testing.T) {
	const actorURL = "https://peer.example.com/ap/actor"
	const inboxURL = "https://peer.example.com/ap/inbox"

	ctx := context.Background()

	var acceptedURL string
	fed := &mockAPFederator{
		sendAcceptFn: func(_ context.Context, followerActorURL string) error {
			acceptedURL = followerActorURL
			return nil
		},
	}
	s := testServerWithMock(t, fed)
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	require.NoError(t, s.db.AddFollowRequest(ctx, actorURL, "pubkey-peer", inboxURL, nil))

	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	body := `{"target":"` + actorURL + `"}`
	req, _ := http.NewRequest("POST", srv.URL+"/api/admin/follows/accept", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	require.Equal(t, actorURL, result["accepted"])
	require.Equal(t, actorURL, acceptedURL)
}

func TestAdminAcceptFollowMutualFollowBack(t *testing.T) {
	const actorURL = "https://peer.example.com/ap/actor"
	const inboxURL = "https://peer.example.com/ap/inbox"

	ctx := context.Background()

	var followBackActor string
	fed := &mockAPFederator{
		sendAcceptFn: func(_ context.Context, _ string) error { return nil },
		sendFollowFn: func(_ context.Context, actor string) (string, error) {
			followBackActor = actor
			return actor, nil
		},
	}
	s := testServerWithMock(t, fed)
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	s.cfg.Federation.AutoAccept = activitypub.AutoAcceptMutual
	require.NoError(t, s.db.AddFollowRequest(ctx, actorURL, "pubkey-peer", inboxURL, nil))

	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	body := `{"target":"` + actorURL + `"}`
	req, _ := http.NewRequest("POST", srv.URL+"/api/admin/follows/accept", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	require.Equal(t, actorURL, result["accepted"])
	require.Equal(t, actorURL, result["followed_back"])
	require.Equal(t, actorURL, followBackActor, "SendFollow should be called with the accepted actor")
}

func TestAdminAcceptFollowMutualSkipsExistingOutgoing(t *testing.T) {
	const actorURL = "https://peer.example.com/ap/actor"
	const inboxURL = "https://peer.example.com/ap/inbox"

	ctx := context.Background()

	followCalled := false
	fed := &mockAPFederator{
		sendAcceptFn: func(_ context.Context, _ string) error { return nil },
		sendFollowFn: func(_ context.Context, _ string) (string, error) {
			followCalled = true
			return "", nil
		},
	}
	s := testServerWithMock(t, fed)
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	s.cfg.Federation.AutoAccept = activitypub.AutoAcceptMutual
	require.NoError(t, s.db.AddFollowRequest(ctx, actorURL, "pubkey-peer", inboxURL, nil))
	// Already following this actor.
	require.NoError(t, s.db.AddOutgoingFollow(ctx, actorURL))

	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	body := `{"target":"` + actorURL + `"}`
	req, _ := http.NewRequest("POST", srv.URL+"/api/admin/follows/accept", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	require.Equal(t, actorURL, result["accepted"])
	require.Empty(t, result["followed_back"], "should not follow back when outgoing follow already exists")
	require.False(t, followCalled, "SendFollow should not be called when outgoing follow already exists")
}

func TestAdminRejectFollowMissingTarget(t *testing.T) {
	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/api/admin/follows/reject", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestAdminRejectFollowSendRejectError(t *testing.T) {
	const actorURL = "https://peer.example.com/ap/actor"
	fed := &mockAPFederator{
		sendRejectFn: func(_ context.Context, _ string) error {
			return errors.New("no pending request")
		},
	}
	s := testServerWithMock(t, fed)
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	body := `{"target":"` + actorURL + `"}`
	req, _ := http.NewRequest("POST", srv.URL+"/api/admin/follows/reject", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

func TestAdminRejectFollowSuccess(t *testing.T) {
	const actorURL = "https://peer.example.com/ap/actor"
	const inboxURL = "https://peer.example.com/ap/inbox"

	ctx := context.Background()

	var rejectedURL string
	fed := &mockAPFederator{
		sendRejectFn: func(_ context.Context, followerActorURL string) error {
			rejectedURL = followerActorURL
			return nil
		},
	}
	s := testServerWithMock(t, fed)
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	require.NoError(t, s.db.AddFollowRequest(ctx, actorURL, "pubkey-peer", inboxURL, nil))

	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	body := `{"target":"` + actorURL + `"}`
	req, _ := http.NewRequest("POST", srv.URL+"/api/admin/follows/reject", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	require.Equal(t, actorURL, result["rejected"])
	require.Equal(t, actorURL, rejectedURL)
}

func TestAdminRemoveFollowMissingTarget(t *testing.T) {
	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	req, _ := http.NewRequest("DELETE", srv.URL+testFollowsAPI, strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestAdminRemoveFollowNotFound(t *testing.T) {
	const actorURL = "https://peer.example.com/ap/actor"
	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	body := `{"target":"` + actorURL + `"}`
	req, _ := http.NewRequest("DELETE", srv.URL+testFollowsAPI, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

func TestAdminRemoveFollowSuccess(t *testing.T) {
	const actorURL = "https://peer.example.com/ap/actor"
	ctx := context.Background()

	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	require.NoError(t, s.db.AddFollow(ctx, actorURL, "pubkey-peer", "https://peer.example.com", nil))

	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	body := `{"target":"` + actorURL + `"}`
	req, _ := http.NewRequest("DELETE", srv.URL+testFollowsAPI, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	require.Equal(t, actorURL, result["removed"])

	f, err := s.db.GetFollow(ctx, actorURL)
	require.NoError(t, err)
	require.Nil(t, f, "follow should be removed from the DB")
}

func TestAdminRemoveFollowOnlyOutgoing(t *testing.T) {
	const actorURL = "https://peer.example.com/ap/actor"
	ctx := context.Background()

	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	require.NoError(t, s.db.AddOutgoingFollow(ctx, actorURL))

	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	body := `{"target":"` + actorURL + `"}`
	req, _ := http.NewRequest("DELETE", srv.URL+testFollowsAPI, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	of, err := s.db.GetOutgoingFollow(ctx, actorURL)
	require.NoError(t, err)
	require.Nil(t, of, "outgoing follow should be removed")
}

func TestAdminRemoveFollowBothTables(t *testing.T) {
	const actorURL = "https://peer.example.com/ap/actor"
	ctx := context.Background()

	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	require.NoError(t, s.db.AddFollow(ctx, actorURL, "pubkey-peer", "https://peer.example.com", nil))
	require.NoError(t, s.db.AddOutgoingFollow(ctx, actorURL))

	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	body := `{"target":"` + actorURL + `"}`
	req, _ := http.NewRequest("DELETE", srv.URL+testFollowsAPI, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	f, err := s.db.GetFollow(ctx, actorURL)
	require.NoError(t, err)
	require.Nil(t, f, "inbound follow should be removed")

	of, err := s.db.GetOutgoingFollow(ctx, actorURL)
	require.NoError(t, err)
	require.Nil(t, of, "outgoing follow should be removed")
}

func TestAdminRemoveFollowForceRemovesDespiteUnreachablePeer(t *testing.T) {
	const actorURL = "https://peer.example.com/ap/actor"
	ctx := context.Background()

	fed := &mockAPFederator{
		resolveFollowTargetFn: func(_ context.Context, _ string) (string, error) {
			return "", errors.New("peer unreachable")
		},
	}
	s := testServerWithMock(t, fed)
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	require.NoError(t, s.db.AddFollow(ctx, actorURL, "pubkey-peer", "https://peer.example.com", nil))

	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	body := `{"target":"` + actorURL + `","force":true}`
	req, _ := http.NewRequest("DELETE", srv.URL+testFollowsAPI, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	f, err := s.db.GetFollow(ctx, actorURL)
	require.NoError(t, err)
	require.Nil(t, f)

	a, err := s.db.GetActor(ctx, actorURL)
	require.NoError(t, err)
	require.Nil(t, a)
}

func TestAdminRemoveFollowWithoutForceFailsOnUnreachablePeer(t *testing.T) {
	const actorURL = "https://peer.example.com/ap/actor"
	ctx := context.Background()

	fed := &mockAPFederator{
		resolveFollowTargetFn: func(_ context.Context, _ string) (string, error) {
			return "", errors.New("peer unreachable")
		},
	}
	s := testServerWithMock(t, fed)
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	require.NoError(t, s.db.AddFollow(ctx, actorURL, "pubkey-peer", "https://peer.example.com", nil))

	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	body := `{"target":"` + actorURL + `"}`
	req, _ := http.NewRequest("DELETE", srv.URL+testFollowsAPI, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

func TestAdminAddFollowCreatesOutgoingNotInbound(t *testing.T) {
	const actorURL = "https://peer.example.com/ap/actor"
	const inboxURL = "https://peer.example.com/ap/inbox"

	fed := &mockAPFederator{
		fetchActorFn: func(_ context.Context, _ string) (*activitypub.Actor, error) {
			return adminActor(actorURL, inboxURL), nil
		},
		sendFollowFn: func(_ context.Context, target string) (string, error) {
			return target, nil
		},
	}
	s := testServerWithMock(t, fed)
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	body := `{"target":"` + actorURL + `"}`
	req, _ := http.NewRequest("POST", srv.URL+testFollowsAPI, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	ctx := context.Background()

	// Must be in outgoing_follows.
	of, err := s.db.GetOutgoingFollow(ctx, actorURL)
	require.NoError(t, err)
	require.NotNil(t, of, "outgoing follow should be persisted")

	// Must NOT be in follow_requests (that's for inbound requests).
	fr, err := s.db.GetFollowRequest(ctx, actorURL)
	require.NoError(t, err)
	require.Nil(t, fr, "follow_requests should not have outgoing follow")
}

func TestAdminAllEndpointsRequireAuth(t *testing.T) {
	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	type endpoint struct {
		method string
		path   string
		body   string
	}
	endpoints := []endpoint{
		{http.MethodGet, "/api/admin/identity", ""},
		{http.MethodGet, "/api/admin/images", ""},
		{http.MethodGet, testFollowsAPI, ""},
		{http.MethodGet, "/api/admin/follows/pending", ""},
		{http.MethodPost, testFollowsAPI, testFollowBody},
		{http.MethodPost, "/api/admin/follows/accept", testFollowBody},
		{http.MethodPost, "/api/admin/follows/reject", testFollowBody},
		{http.MethodDelete, testFollowsAPI, testFollowBody},
	}

	for _, ep := range endpoints {
		req, _ := http.NewRequest(ep.method, srv.URL+ep.path, strings.NewReader(ep.body))
		if ep.body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode,
			"%s %s without auth should return 401", ep.method, ep.path)
	}
}

func TestAdminAllEndpointsRejectWrongToken(t *testing.T) {
	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = "correct-token"
	s.cfg.AdminToken = "correct-token"
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+testFollowsAPI, nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestAdminListOutgoingFollowsEmpty(t *testing.T) {
	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/api/admin/follows/outgoing", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var follows []any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&follows))
	require.Empty(t, follows)
}

func TestAdminListOutgoingFollowsWithData(t *testing.T) {
	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	ctx := context.Background()

	// Add some outgoing follows with different statuses
	require.NoError(t, s.db.AddOutgoingFollow(ctx, "https://pending.example.com/ap/actor"))
	require.NoError(t, s.db.AddOutgoingFollow(ctx, "https://accepted.example.com/ap/actor"))
	require.NoError(t, s.db.AcceptOutgoingFollow(ctx, "https://accepted.example.com/ap/actor"))
	require.NoError(t, s.db.AddOutgoingFollow(ctx, "https://rejected.example.com/ap/actor"))
	require.NoError(t, s.db.RejectOutgoingFollow(ctx, "https://rejected.example.com/ap/actor"))

	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	// Test listing all
	req, _ := http.NewRequest("GET", srv.URL+"/api/admin/follows/outgoing", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var follows []map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&follows))
	require.Len(t, follows, 3)
}

func TestAdminListOutgoingFollowsFilterByStatus(t *testing.T) {
	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	ctx := context.Background()

	require.NoError(t, s.db.AddOutgoingFollow(ctx, "https://pending1.example.com/ap/actor"))
	require.NoError(t, s.db.AddOutgoingFollow(ctx, "https://pending2.example.com/ap/actor"))
	require.NoError(t, s.db.AddOutgoingFollow(ctx, "https://accepted.example.com/ap/actor"))
	require.NoError(t, s.db.AcceptOutgoingFollow(ctx, "https://accepted.example.com/ap/actor"))

	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	// Test filtering by pending status
	req, _ := http.NewRequest("GET", srv.URL+"/api/admin/follows/outgoing?status=pending", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var follows []map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&follows))
	require.Len(t, follows, 2)
	for _, f := range follows {
		require.Equal(t, "pending", f["we_follow_status"])
	}
}

func TestAdminListImagesEmpty(t *testing.T) {
	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.AdminToken = testToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/api/admin/images", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	var images []any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&images))
	require.Empty(t, images)
}

func TestAdminListImagesWithData(t *testing.T) {
	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.AdminToken = testToken
	ctx := context.Background()

	repo, err := s.db.GetOrCreateRepository(ctx, "docker.io/app/image", "https://alice.example.com/ap/actor")
	require.NoError(t, err)
	v := &database.PackageVersion{PackageID: repo.ID, Version: "sha256:abc", Metadata: []byte(`{}`)}
	require.NoError(t, s.db.PutPackageVersion(ctx, v))
	mt := "application/vnd.oci.image.layer.v1.tar+gzip"
	require.NoError(t, s.db.PutBlob(ctx, "sha256:layer1", 2048, &mt, true))
	require.NoError(t, s.db.PutBlobReferences(ctx, v.ID, []database.BlobRef{{Digest: "sha256:layer1", Size: 2048, MediaType: &mt}}))
	require.NoError(t, s.db.PutPackageTag(ctx, repo.ID, testTagLatest, v.Version))

	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/api/admin/images", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var images []struct {
		Name      string   `json:"name"`
		Tags      []string `json:"tags"`
		SizeBytes int64    `json:"size_bytes"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&images))
	require.Len(t, images, 1)
	require.Equal(t, "docker.io/app/image", images[0].Name)
	require.Equal(t, int64(2048), images[0].SizeBytes)
	require.Equal(t, []string{testTagLatest}, images[0].Tags)
}

func TestAdminEvictMirrorWholeRepo(t *testing.T) {
	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	ctx := context.Background()

	const remote = "https://remote.example.com/ap/actor"
	repo, err := s.db.GetOrCreateRepository(ctx, "ghcr.io/user/mirrored", remote)
	require.NoError(t, err)
	manifestDigest := "sha256:" + strings.Repeat("a", 64)
	require.NoError(t, s.db.PutManifest(ctx, &database.Manifest{
		RepositoryID: repo.ID,
		Digest:       manifestDigest,
		MediaType:    "application/vnd.oci.image.manifest.v1+json",
		Content:      []byte(`{}`),
	}))

	layerDigest, layerSize, err := s.blobs.Put(ctx, strings.NewReader("layerdata-whole"), "")
	require.NoError(t, err)
	require.NoError(t, s.db.PutBlob(ctx, layerDigest, layerSize, nil, true))
	man, err := s.db.GetManifestByDigest(ctx, repo.ID, manifestDigest)
	require.NoError(t, err)
	require.NotNil(t, man)
	require.NoError(t, s.db.PutManifestLayers(ctx, man.ID, []database.BlobRef{
		{Digest: layerDigest, Size: layerSize},
	}))

	beforeActs, err := s.db.ListActivitiesPage(ctx, s.identity.ActorURL, 0, 50)
	require.NoError(t, err)

	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/admin/mirrors/ghcr.io/user/mirrored", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	gone, err := s.db.GetRepository(ctx, "ghcr.io/user/mirrored")
	require.NoError(t, err)
	require.Nil(t, gone, "mirror repo row should be gone")

	exists, _ := s.blobs.Exists(ctx, layerDigest)
	require.False(t, exists, "blob bytes should be gone")
	b, _ := s.db.GetBlob(ctx, layerDigest)
	require.Nil(t, b, "blob row should be gone")

	afterActs, err := s.db.ListActivitiesPage(ctx, s.identity.ActorURL, 0, 50)
	require.NoError(t, err)
	require.Equal(t, len(beforeActs), len(afterActs), "eviction must not emit AP activities")
}

func TestAdminEvictMirrorSingleManifest(t *testing.T) {
	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	ctx := context.Background()

	const remote = "https://remote.example.com/ap/actor"
	repo, err := s.db.GetOrCreateRepository(ctx, "ghcr.io/user/mirrored", remote)
	require.NoError(t, err)
	dgst := "sha256:" + strings.Repeat("b", 64)
	require.NoError(t, s.db.PutManifest(ctx, &database.Manifest{
		RepositoryID: repo.ID,
		Digest:       dgst,
		MediaType:    "application/vnd.oci.image.manifest.v1+json",
		Content:      []byte(`{}`),
	}))

	layerDigest, layerSize, err := s.blobs.Put(ctx, strings.NewReader("layerdata-single"), "")
	require.NoError(t, err)
	require.NoError(t, s.db.PutBlob(ctx, layerDigest, layerSize, nil, true))
	man, err := s.db.GetManifestByDigest(ctx, repo.ID, dgst)
	require.NoError(t, err)
	require.NotNil(t, man)
	require.NoError(t, s.db.PutManifestLayers(ctx, man.ID, []database.BlobRef{
		{Digest: layerDigest, Size: layerSize},
	}))

	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/admin/mirrors/ghcr.io/user/mirrored?digest="+dgst, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	got, err := s.db.GetManifestByDigest(ctx, repo.ID, dgst)
	require.NoError(t, err)
	require.Nil(t, got)
	stillThere, err := s.db.GetRepository(ctx, "ghcr.io/user/mirrored")
	require.NoError(t, err)
	require.NotNil(t, stillThere, "repo row should remain after per-manifest evict")

	exists, _ := s.blobs.Exists(ctx, layerDigest)
	require.False(t, exists, "blob bytes should be gone")
	b, _ := s.db.GetBlob(ctx, layerDigest)
	require.Nil(t, b, "blob row should be gone")
}

func TestAdminEvictMirrorRejectsLocallyOwned(t *testing.T) {
	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	ctx := context.Background()

	_, err := s.db.GetOrCreateRepository(ctx, "test.example.com/local", s.identity.ActorURL)
	require.NoError(t, err)

	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/admin/mirrors/test.example.com/local", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusBadRequest, resp.StatusCode, "locally owned repo must be rejected")

	got, err := s.db.GetRepository(ctx, "test.example.com/local")
	require.NoError(t, err)
	require.NotNil(t, got, "rejected eviction must not delete the repo")
}

func TestAdminEvictMirrorNotFound(t *testing.T) {
	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/admin/mirrors/ghcr.io/user/nonexistent", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestAdminRunGCRemovesOrphanMetadata(t *testing.T) {
	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	ctx := context.Background()

	const orphanDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	require.NoError(t, s.db.PutBlob(ctx, orphanDigest, 42, nil, false))

	got, err := s.db.GetBlob(ctx, orphanDigest)
	require.NoError(t, err)
	require.NotNil(t, got, "orphan metadata should be present before GC")

	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/admin/gc", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	got, err = s.db.GetBlob(ctx, orphanDigest)
	require.NoError(t, err)
	require.Nil(t, got, "orphan metadata should be removed by GC")
}

func TestAdminRunGCRequiresAuth(t *testing.T) {
	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/admin/gc", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestAdminEvictMirrorRequiresAuth(t *testing.T) {
	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/admin/mirrors/anything", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestAdminListImagesInternalError(t *testing.T) {
	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.AdminToken = testToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	_ = s.db.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/api/admin/images", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

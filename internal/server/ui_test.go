package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/activitypub"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/blobstore"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/config"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
)

func testServerWithUI(t *testing.T, uiEnabled bool) *Server {
	t.Helper()
	return testServerWithAccountDomain(t, uiEnabled, testDomain)
}

// testServerWithAccountDomain builds a UI test server whose AccountDomain (the
// registry namespace normalizeRepo prepends) may differ from its endpoint
// Domain. Passing testDomain reproduces the default single-domain deployment.
func testServerWithAccountDomain(t *testing.T, uiEnabled bool, accountDomain string) *Server {
	t.Helper()
	return testServerWithDomains(t, uiEnabled, "https://test.example.com", testDomain, accountDomain)
}

// testServerWithDomains builds a UI test server with independently-set endpoint,
// endpoint Domain, and AccountDomain, so tests can reproduce deployments where
// the endpoint host, the federation Domain, and the registry namespace differ.
// (The production config loader derives Domain from the endpoint host and
// defaults AccountDomain to it, so these only diverge under direct construction.)
func testServerWithDomains(t *testing.T, uiEnabled bool, endpoint, domain, accountDomain string) *Server {
	t.Helper()
	dir := t.TempDir()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	db, err := database.OpenSQLite(dir, 0, 0, logger)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	blobs, err := blobstore.New(dir, logger)
	require.NoError(t, err)

	identity, err := activitypub.LoadOrCreateIdentity(endpoint, domain, accountDomain, "", logger)
	require.NoError(t, err)

	gcEnabled := true
	cfg := &config.Config{
		Name:          "test-node",
		Endpoint:      endpoint,
		Domain:        domain,
		AccountDomain: accountDomain,
		Listen:        ":0",
		RegistryToken: "test-token",
		Peering: config.Peering{
			HealthCheckInterval: 30 * time.Second,
			FetchTimeout:        10 * time.Second,
		},
		Limits: config.Limits{
			MaxManifestSize: config.DefaultMaxManifestSize,
			MaxBlobSize:     config.DefaultMaxBlobSize,
		},
		RateLimits: config.RateLimits{
			InboxRate:         1000,
			InboxBurst:        1000,
			RegistryPushRate:  1000,
			RegistryPushBurst: 1000,
		},
		GC: config.GC{
			Enabled:          &gcEnabled,
			Interval:         6 * time.Hour,
			StalePeerBlobAge: 30 * 24 * time.Hour,
			OrphanBatchSize:  500,
		},
		UI: config.UI{
			Enabled: uiEnabled,
		},
	}

	s, err := New(cfg, db, blobs, identity, "test", logger)
	require.NoError(t, err)
	return s
}

func TestUIDisabled(t *testing.T) {
	s := testServerWithUI(t, false)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "application/json")

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), `"status":"ok"`)
}

func TestUIIndex(t *testing.T) {
	s := testServerWithUI(t, true)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	// Create a local repo so we can test the "My Images" section
	_, err := s.db.GetOrCreateRepository(t.Context(), "test.example.com/localapp", s.identity.ActorURL)
	require.NoError(t, err)

	resp, err := http.Get(srv.URL + "/")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/html")

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), "test-node")
	assert.Contains(t, string(body), "My Images")
	// Locally-owned repo is shown without the doubled instance domain.
	assert.Contains(t, string(body), "<strong>localapp</strong>")
	assert.NotContains(t, string(body), testDomain+"/"+testDomain)
}

func TestUISearch(t *testing.T) {
	s := testServerWithUI(t, true)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	// Create a test repo
	_, err := s.db.GetOrCreateRepository(t.Context(), "test.example.com/myapp", s.identity.ActorURL)
	require.NoError(t, err)

	resp, err := http.Get(srv.URL + "/ui/search?q=myapp")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	// Locally-owned repo is shown without the doubled instance domain.
	assert.Contains(t, string(body), "<strong>myapp</strong>")
	assert.NotContains(t, string(body), testDomain+"/"+testDomain)
}

func TestUISearchShortQuery(t *testing.T) {
	s := testServerWithUI(t, true)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/ui/search?q=a")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	// Short query returns empty 200
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Empty(t, strings.TrimSpace(string(body)))
}

func TestUIStaticAssets(t *testing.T) {
	s := testServerWithUI(t, true)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	tests := []struct {
		path string
	}{
		{"/ui/static/pico.min.css"},
		{"/ui/static/htmx.min.js"},
	}

	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			resp, err := http.Get(srv.URL + tc.path)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()

			assert.Equal(t, http.StatusOK, resp.StatusCode)

			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			assert.NotEmpty(t, body)
		})
	}
}

func TestBuildIndexDataStripsLocalDomainPrefix(t *testing.T) {
	s := testServerWithUI(t, true)
	self := s.identity.ActorURL

	reposPage := &database.ReposPage{
		Repos: []database.RepoWithStats{
			// Locally-owned, domain-prefixed (as normalizeRepo stores it).
			{Name: testDomain + "/wreckroll", OwnerID: self, Tags: []string{testTagLatest}},
			// Locally-owned but NOT domain-prefixed (edge case, invariant 4).
			{Name: "weirdapp", OwnerID: self},
			// Federated repo owned by a peer (invariant 2).
			{Name: "peer.example.dev/user/app", OwnerID: "https://peer.example.dev/ap/actor"},
		},
		TotalCount: 3,
		Page:       1,
		TotalPages: 1,
	}

	data := s.buildIndexData(reposPage, "", 0, 0)

	require.Len(t, data.LocalRepos, 2)
	// Invariant 1: local domain-prefixed repo displays without the instance domain.
	assert.Equal(t, "wreckroll", data.LocalRepos[0].Name)
	// Invariant 4: local repo without the domain prefix is unchanged.
	assert.Equal(t, "weirdapp", data.LocalRepos[1].Name)

	// Invariant 2: federated repo name is displayed unchanged.
	require.Len(t, data.FederatedGroups, 1)
	require.Len(t, data.FederatedGroups[0].Repos, 1)
	assert.Equal(t, "peer.example.dev/user/app", data.FederatedGroups[0].Repos[0].Name)
}

func TestUIRepoTagsStripsLocalDomainPrefix(t *testing.T) {
	s := testServerWithUI(t, true)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	// Stored under the domain-prefixed name (as normalizeRepo writes it).
	_, err := s.db.GetOrCreateRepository(t.Context(), testDomain+"/wreckroll", s.identity.ActorURL)
	require.NoError(t, err)

	// Invariant 3: the index links to the stripped name; it must re-resolve.
	resp, err := http.Get(srv.URL + "/ui/tags/wreckroll")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	// Heading shows the stripped name, and there is no doubled instance domain.
	assert.Contains(t, string(body), "<h1>wreckroll</h1>")
	assert.NotContains(t, string(body), testDomain+"/"+testDomain)
}

// TestUISplitAccountDomainStripsPrefix covers deployments where AccountDomain
// (the registry namespace normalizeRepo prepends) differs from the endpoint
// Domain. Local repos are stored as <accountDomain>/<name>, so the display
// strip and the tags retry must use AccountDomain, not Domain.
func TestUISplitAccountDomainStripsPrefix(t *testing.T) {
	const accountDomain = "account.example.com" // != testDomain (endpoint domain)
	s := testServerWithAccountDomain(t, true, accountDomain)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	// Stored under the AccountDomain-prefixed name (as normalizeRepo writes it).
	_, err := s.db.GetOrCreateRepository(t.Context(), accountDomain+"/wreckroll", s.identity.ActorURL)
	require.NoError(t, err)

	// Index: the pull command shows the stripped name against the endpoint host.
	resp, err := http.Get(srv.URL + "/")
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(body), "<strong>wreckroll</strong>")
	assert.NotContains(t, string(body), accountDomain+"/wreckroll")

	// Tags: /ui/tags/wreckroll must re-resolve the stored AccountDomain-prefixed
	// repo and display the stripped heading.
	tagsResp, err := http.Get(srv.URL + "/ui/tags/wreckroll")
	require.NoError(t, err)
	tagsBody, err := io.ReadAll(tagsResp.Body)
	require.NoError(t, err)
	_ = tagsResp.Body.Close()
	assert.Equal(t, http.StatusOK, tagsResp.StatusCode)
	assert.Contains(t, string(tagsBody), "<h1>wreckroll</h1>")
	assert.NotContains(t, string(tagsBody), accountDomain+"/wreckroll")
}

// TestUIUnsetAccountDomainDivergentDomain reproduces the divergence the
// single-source refactor closes: AccountDomain is unset while the endpoint host
// differs from the federation Domain. With AccountDomain empty, normalizeRepo
// falls back to the actor-URL host (the endpoint host) and namespaces local
// repos under it, while identity.AccountDomain falls back to Domain. The UI must
// strip the prefix normalizeRepo actually prepended (the registry namespace =
// endpoint host), not identity.AccountDomain (= Domain), or display and the tags
// retry both break.
func TestUIUnsetAccountDomainDivergentDomain(t *testing.T) {
	const (
		endpoint     = "https://registry.example.com"
		endpointHost = "registry.example.com"
		domain       = "social.example.com" // != endpointHost; identity.AccountDomain defaults here
	)
	s := testServerWithDomains(t, true, endpoint, domain, "")
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	// normalizeRepo namespaces local repos under the endpoint host when
	// AccountDomain is unset. Store the repo exactly as it would.
	_, err := s.db.GetOrCreateRepository(t.Context(), endpointHost+"/wreckroll", s.identity.ActorURL)
	require.NoError(t, err)

	// Index: the local repo displays stripped, and the pull command is not
	// doubled (RegistryHost is the endpoint host, so an un-stripped name would
	// render "registry.example.com/registry.example.com/wreckroll").
	resp, err := http.Get(srv.URL + "/")
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(body), "<strong>wreckroll</strong>")
	assert.NotContains(t, string(body), endpointHost+"/"+endpointHost)

	// Tags: /ui/tags/wreckroll must re-resolve the endpoint-host-prefixed repo
	// and display the stripped heading.
	tagsResp, err := http.Get(srv.URL + "/ui/tags/wreckroll")
	require.NoError(t, err)
	tagsBody, err := io.ReadAll(tagsResp.Body)
	require.NoError(t, err)
	_ = tagsResp.Body.Close()
	assert.Equal(t, http.StatusOK, tagsResp.StatusCode)
	assert.Contains(t, string(tagsBody), "<h1>wreckroll</h1>")
	assert.NotContains(t, string(tagsBody), endpointHost+"/"+endpointHost)
}

func TestUIRepoTagsFederatedUnchanged(t *testing.T) {
	s := testServerWithUI(t, true)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	_, err := s.db.GetOrCreateRepository(t.Context(), "peer.example.dev/user/app", "https://peer.example.dev/ap/actor")
	require.NoError(t, err)

	// Invariant 2: federated repo path resolves and displays unchanged.
	resp, err := http.Get(srv.URL + "/ui/tags/peer.example.dev/user/app")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), "<h1>peer.example.dev/user/app</h1>")
}

// TestUIIndexGuardsDottedSegmentAndDisclosure covers both changes on the index
// path: the dotted-segment guard (a local repo whose stripped first segment has
// a dot keeps its full, resolving name) and the advanced fully-qualified pull
// disclosure (shown only for repos whose name was actually stripped).
func TestUIIndexGuardsDottedSegmentAndDisclosure(t *testing.T) {
	s := testServerWithUI(t, true)
	self := s.identity.ActorURL

	reposPage := &database.ReposPage{
		Repos: []database.RepoWithStats{
			// Simple local repo: strips to the bare name; the bare pull resolves.
			{Name: testDomain + "/myapp", OwnerID: self, Tags: []string{testTagLatest}},
			// Dotted local repo: stripping would yield "sub.dom/app", whose bare
			// pull does NOT resolve (normalizeRepo sees the dot and won't
			// re-prepend). The guard must keep the full name.
			{Name: testDomain + "/sub.dom/app", OwnerID: self, Tags: []string{testTagLatest}},
		},
		TotalCount: 2,
		Page:       1,
		TotalPages: 1,
	}

	data := s.buildIndexData(reposPage, "", 0, 0)
	require.Len(t, data.LocalRepos, 2)

	// Simple repo: Name stripped, FullName retains the stored prefix.
	assert.Equal(t, "myapp", data.LocalRepos[0].Name)
	assert.Equal(t, testDomain+"/myapp", data.LocalRepos[0].FullName)
	// Dotted repo: guard keeps the full name so the displayed pull resolves.
	assert.Equal(t, testDomain+"/sub.dom/app", data.LocalRepos[1].Name)
	assert.Equal(t, testDomain+"/sub.dom/app", data.LocalRepos[1].FullName)

	rec := httptest.NewRecorder()
	s.renderTemplate(rec, "_repo_list.html.tmpl", data)
	body := rec.Body.String()

	// Simple repo: bare primary command plus a fully-qualified disclosure.
	assert.Contains(t, body, "docker pull "+testDomain+"/myapp:latest")
	assert.Contains(t, body, "<details")
	assert.Contains(t, body, "docker pull "+testDomain+"/"+testDomain+"/myapp:latest")

	// Dotted repo: the primary command is already the fully-qualified, resolving
	// form, so it must render as such.
	assert.Contains(t, body, "docker pull "+testDomain+"/"+testDomain+"/sub.dom/app:latest")

	// Only the stripped (simple) repo gets a disclosure; the dotted repo — whose
	// primary already equals its fully-qualified form — does not.
	assert.Equal(t, 1, strings.Count(body, "<details"))
}

// TestUIRepoTagsAdvancedPullDisclosure covers the fully-qualified pull disclosure
// on the tags page: present for a stripped local repo, absent for a federated
// repo whose displayed name already equals its fully-qualified form.
func TestUIRepoTagsAdvancedPullDisclosure(t *testing.T) {
	s := testServerWithUI(t, true)

	// Simple local repo: primary is bare, disclosure exposes the qualified form.
	local := RepoTagsData{
		RegistryHost: testDomain,
		RepoName:     "myapp",
		FullRepoName: testDomain + "/myapp",
		Tags:         []TagView{{Name: testTagLatest}},
	}
	rec := httptest.NewRecorder()
	s.renderTemplate(rec, "repo_tags.html.tmpl", local)
	body := rec.Body.String()
	assert.Contains(t, body, "docker pull "+testDomain+"/myapp:latest")
	assert.Contains(t, body, "<details")
	assert.Contains(t, body, "docker pull "+testDomain+"/"+testDomain+"/myapp:latest")

	// Federated repo: RepoName already equals FullRepoName, so no disclosure.
	fed := RepoTagsData{
		RegistryHost: testDomain,
		RepoName:     "bar.com/app",
		FullRepoName: "bar.com/app",
		Tags:         []TagView{{Name: testTagLatest}},
	}
	rec2 := httptest.NewRecorder()
	s.renderTemplate(rec2, "repo_tags.html.tmpl", fed)
	body2 := rec2.Body.String()
	assert.NotContains(t, body2, "<details")
	assert.Contains(t, body2, "docker pull "+testDomain+"/bar.com/app:latest")
}

func TestHumanizeBytes(t *testing.T) {
	tests := []struct {
		bytes    int64
		expected string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1048576, "1.0 MB"},
		{1073741824, "1.0 GB"},
	}

	for _, tc := range tests {
		t.Run(tc.expected, func(t *testing.T) {
			assert.Equal(t, tc.expected, humanizeBytes(tc.bytes))
		})
	}
}

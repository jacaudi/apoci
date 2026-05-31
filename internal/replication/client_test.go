package replication

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseChallenge(t *testing.T) {
	ch := parseChallenge(`Bearer realm="https://auth.example.com/token",service="registry.example.com"`)
	require.Equal(t, "https://auth.example.com/token", ch.realm)
	require.Equal(t, "registry.example.com", ch.service)

	require.Empty(t, parseChallenge("Basic realm=x").realm)
	require.Empty(t, parseChallenge("").realm)
}

func TestDestRepo(t *testing.T) {
	tests := []struct {
		name   string
		target Target
		local  string
		want   string
	}{
		{"passthrough", Target{}, testLocalRepo, testLocalRepo},
		{"strip prefix", Target{StripPrefix: testStripPrefix}, testLocalRepo, "team/app"},
		{"dest namespace", Target{DestNamespace: "myorg"}, "team/app", "myorg/team/app"},
		{"strip + namespace", Target{StripPrefix: testStripPrefix, DestNamespace: "myorg"}, testLocalRepo, "myorg/team/app"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.target.DestRepo(tt.local))
		})
	}
}

func TestMatchRepo(t *testing.T) {
	require.True(t, matchRepo(Target{}, "any/repo"), "no globs matches all")
	require.True(t, matchRepo(Target{RepoGlobs: []string{"team/*"}}, "team/app"))
	require.False(t, matchRepo(Target{RepoGlobs: []string{"team/*"}}, "other/app"))
}

func TestResolveUpload(t *testing.T) {
	c := NewClient(Target{Endpoint: "https://ghcr.io"}, 0)

	abs, err := c.resolveUpload("/v2/org/app/blobs/uploads/uuid-123", "sha256:abc")
	require.NoError(t, err)
	require.Equal(t, "https://ghcr.io/v2/org/app/blobs/uploads/uuid-123?digest=sha256%3Aabc", abs)

	// Already-absolute Location with an existing query is preserved.
	abs2, err := c.resolveUpload("https://up.ghcr.io/v2/x/blobs/uploads/u?_state=z", "sha256:def")
	require.NoError(t, err)
	require.Contains(t, abs2, "https://up.ghcr.io/v2/x/blobs/uploads/u?")
	require.Contains(t, abs2, "digest=sha256%3Adef")
	require.Contains(t, abs2, "_state=z")
}

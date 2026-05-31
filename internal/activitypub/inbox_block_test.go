package activitypub

import (
	"testing"

	"github.com/stretchr/testify/require"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
)

const (
	blockPeerDomain = "peer.example.com"
	blockEvilDomain = "evil.example.com"
	blockPeerActor  = "https://peer.example.com/ap/actor"
)

func newBlockTestHandler(t *testing.T, cfg InboxConfig) *InboxHandler {
	t.Helper()
	dir := t.TempDir()
	db, err := database.OpenSQLite(dir, 0, 0, discardLogger())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	id, _ := LoadOrCreateIdentity("https://bob.example.com", "bob.example.com", "", "", discardLogger())
	h := NewInboxHandler(id, db, cfg, discardLogger())
	t.Cleanup(h.Stop)
	return h
}

func TestBlocklistFromConfig(t *testing.T) {
	h := newBlockTestHandler(t, InboxConfig{
		BlockedDomains: []string{blockEvilDomain},
		BlockedActors:  []string{"https://bad.example.com/ap/actor"},
	})

	require.True(t, h.isBlocked("https://evil.example.com/ap/actor"))
	require.True(t, h.isBlocked("https://sub.evil.example.com/ap/actor"), "subdomains are blocked")
	require.True(t, h.isBlocked("https://bad.example.com/ap/actor"))
	require.False(t, h.isBlocked("https://good.example.com/ap/actor"))
}

func TestPauseResumeDomainHotReload(t *testing.T) {
	h := newBlockTestHandler(t, InboxConfig{})

	require.False(t, h.isBlocked(blockPeerActor))

	h.PauseDomain(blockPeerDomain)
	require.True(t, h.isBlocked(blockPeerActor), "pause should take effect without restart")
	require.Equal(t, []string{blockPeerDomain}, h.BlockedDomains())

	h.ResumeDomain(blockPeerDomain)
	require.False(t, h.isBlocked(blockPeerActor), "resume should lift the block")
	require.Empty(t, h.BlockedDomains())
}

func TestPauseResumeActorHotReload(t *testing.T) {
	h := newBlockTestHandler(t, InboxConfig{})

	other := "https://peer.example.com/ap/other"

	h.PauseActor(blockPeerActor)
	require.True(t, h.isBlocked(blockPeerActor))
	require.False(t, h.isBlocked(other), "actor block is exact-match, not domain-wide")
	require.Equal(t, []string{blockPeerActor}, h.BlockedActors())

	h.ResumeActor(blockPeerActor)
	require.False(t, h.isBlocked(blockPeerActor))
}

func TestPauseDomainPreservesConfigBlocks(t *testing.T) {
	h := newBlockTestHandler(t, InboxConfig{BlockedDomains: []string{blockEvilDomain}})

	h.PauseDomain(blockPeerDomain)
	require.True(t, h.isBlocked("https://evil.example.com/ap/actor"), "config block must survive a later pause")
	require.True(t, h.isBlocked(blockPeerActor))
	require.Equal(t, []string{blockEvilDomain, blockPeerDomain}, h.BlockedDomains())
}

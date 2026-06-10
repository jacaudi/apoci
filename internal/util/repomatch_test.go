package util

import "testing"

func TestMatchRepoGlob(t *testing.T) {
	const (
		ns           = "erwanleboucher.dev"
		buildcache   = "*/*-buildcache"
		towonelCache = "erwanleboucher.dev/towonel/towonel-buildcache"
	)
	cases := []struct {
		name string
		glob string
		repo string
		ns   string
		want bool
	}{
		{"relative glob matches prefixed repo", buildcache, towonelCache, ns, true},
		{"relative exact matches prefixed repo", "eleboucher/agentmemory", "erwanleboucher.dev/eleboucher/agentmemory", ns, true},
		{"full glob still matches", "*/*/*-buildcache", towonelCache, ns, true},
		{"non-buildcache does not match", buildcache, "erwanleboucher.dev/towonel/towonel-node", ns, false},
		{"different namespace not stripped", "eleboucher/agentmemory", "other.dev/eleboucher/agentmemory", ns, false},
		{"empty namespace falls back to direct match", buildcache, "towonel/towonel-buildcache", "", true},
		{"empty repo never matches", buildcache, "", ns, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MatchRepoGlob(tc.glob, tc.repo, tc.ns); got != tc.want {
				t.Errorf("MatchRepoGlob(%q, %q, %q) = %v, want %v", tc.glob, tc.repo, tc.ns, got, tc.want)
			}
		})
	}
}

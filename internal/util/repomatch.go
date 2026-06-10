package util

import (
	"path"
	"strings"
)

// MatchRepoGlob matches glob against repo, trying both the stored
// namespace-prefixed name and the namespace-stripped form so globs can be
// written relative to the local namespace (path.Match's '*' never crosses '/').
func MatchRepoGlob(glob, repo, namespace string) bool {
	if matchGlob(glob, repo) {
		return true
	}
	if namespace != "" {
		if rel, ok := strings.CutPrefix(repo, namespace+"/"); ok {
			return matchGlob(glob, rel)
		}
	}
	return false
}

func matchGlob(glob, s string) bool {
	ok, err := path.Match(glob, s)
	return err == nil && ok
}

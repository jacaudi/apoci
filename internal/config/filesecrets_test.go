package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// writeSecret writes a secret file in a temp dir and returns its path.
func writeSecret(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
	return path
}

// Invariant 1: <VAR>_FILE set + bare env unset → var loaded from file contents.
// Invariant 5: file contents are trimmed.
func TestExpandFileSecretsLoadsFromFile(t *testing.T) {
	const name = "APOCI_TEST_SECRET"
	// Register the bare var so the os.Setenv done inside the helper is
	// restored at cleanup and does not leak into other tests.
	t.Setenv(name, "")
	t.Setenv(name+"_FILE", writeSecret(t, "  s3cr3t-value\n"))

	require.NoError(t, expandFileSecrets(name))
	require.Equal(t, "s3cr3t-value", os.Getenv(name))
}

// Invariant 2: bare env set AND _FILE set → bare env wins, file ignored.
func TestExpandFileSecretsBareEnvWins(t *testing.T) {
	const name = "APOCI_TEST_SECRET"
	t.Setenv(name, "explicit-value")
	t.Setenv(name+"_FILE", writeSecret(t, "file-value"))

	require.NoError(t, expandFileSecrets(name))
	require.Equal(t, "explicit-value", os.Getenv(name))
}

// Invariant 3: neither set → no-op, var stays empty (auto-gen fallback preserved).
func TestExpandFileSecretsNoOpWhenUnset(t *testing.T) {
	const name = "APOCI_TEST_SECRET"
	t.Setenv(name, "")
	// Ensure no _FILE is present.
	require.NoError(t, os.Unsetenv(name+"_FILE"))

	require.NoError(t, expandFileSecrets(name))
	require.Empty(t, os.Getenv(name))
}

// Invariant 4: _FILE points at an unreadable/nonexistent path → error (loud failure).
func TestExpandFileSecretsUnreadableFileErrors(t *testing.T) {
	const name = "APOCI_TEST_SECRET"
	t.Setenv(name, "")
	t.Setenv(name+"_FILE", filepath.Join(t.TempDir(), "does-not-exist"))

	err := expandFileSecrets(name)
	require.Error(t, err)
	require.Empty(t, os.Getenv(name), "bare var must remain unset on failure")
}

// End-to-end via Load(): registry token sourced from APOCI_REGISTRY_TOKEN_FILE
// (invariant 1 + 5) instead of being auto-generated.
func TestRegistryTokenFromFile(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, fmt.Sprintf("endpoint: \"https://test.example.com\"\ndataDir: %q\n", dir))

	t.Setenv("APOCI_REGISTRY_TOKEN", "")
	t.Setenv("APOCI_REGISTRY_TOKEN_FILE", writeSecret(t, "file-mounted-token\n"))

	cfg, err := Load(path, true)
	require.NoError(t, err)
	require.Equal(t, "file-mounted-token", cfg.RegistryToken)
}

// End-to-end via Load(): an unreadable _FILE fails loudly instead of silently
// auto-generating a token (invariant 4).
func TestRegistryTokenFileUnreadable(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, fmt.Sprintf("endpoint: \"https://test.example.com\"\ndataDir: %q\n", dir))

	t.Setenv("APOCI_REGISTRY_TOKEN", "")
	t.Setenv("APOCI_REGISTRY_TOKEN_FILE", filepath.Join(t.TempDir(), "missing"))

	_, err := Load(path, true)
	require.Error(t, err)
}

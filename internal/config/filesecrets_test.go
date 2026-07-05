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

// <VAR>_FILE set, bare env unset → var loaded from (trimmed) file contents.
func TestExpandFileSecretsLoadsFromFile(t *testing.T) {
	const name = "APOCI_TEST_SECRET"
	// Register the bare var so the helper's os.Setenv is restored at cleanup.
	t.Setenv(name, "")
	t.Setenv(name+"_FILE", writeSecret(t, "  s3cr3t-value\n"))

	got, err := expandFileSecrets(name)
	require.NoError(t, err)
	require.Equal(t, "s3cr3t-value", os.Getenv(name))
	require.Equal(t, []string{name}, got, "a file-sourced var must be reported")
}

// Bare env set and _FILE set → bare env wins, file ignored.
func TestExpandFileSecretsBareEnvWins(t *testing.T) {
	const name = "APOCI_TEST_SECRET"
	t.Setenv(name, "explicit-value")
	t.Setenv(name+"_FILE", writeSecret(t, "file-value"))

	got, err := expandFileSecrets(name)
	require.NoError(t, err)
	require.Equal(t, "explicit-value", os.Getenv(name))
	require.Empty(t, got, "a directly-set var must not be reported as file-sourced")
}

// Neither set → no-op, var stays empty (auto-gen fallback preserved).
func TestExpandFileSecretsNoOpWhenUnset(t *testing.T) {
	const name = "APOCI_TEST_SECRET"
	t.Setenv(name, "")
	require.NoError(t, os.Unsetenv(name+"_FILE"))

	got, err := expandFileSecrets(name)
	require.NoError(t, err)
	require.Empty(t, os.Getenv(name))
	require.Empty(t, got)
}

// _FILE points at a nonexistent path → error.
func TestExpandFileSecretsUnreadableFileErrors(t *testing.T) {
	const name = "APOCI_TEST_SECRET"
	t.Setenv(name, "")
	t.Setenv(name+"_FILE", filepath.Join(t.TempDir(), "does-not-exist"))

	got, err := expandFileSecrets(name)
	require.Error(t, err)
	require.Nil(t, got)
	require.Empty(t, os.Getenv(name), "bare var must remain unset on failure")
}

// _FILE points at an empty file → error, not a fallthrough to auto-gen.
func TestExpandFileSecretsEmptyFileErrors(t *testing.T) {
	const name = "APOCI_TEST_SECRET"
	t.Setenv(name, "")
	t.Setenv(name+"_FILE", writeSecret(t, ""))

	got, err := expandFileSecrets(name)
	require.Error(t, err)
	require.Contains(t, err.Error(), name+"_FILE", "error must name the var")
	require.Nil(t, got)
	require.Empty(t, os.Getenv(name), "bare var must remain unset on failure")
}

// A whitespace-only file trims to empty and fails like an empty file.
func TestExpandFileSecretsWhitespaceOnlyFileErrors(t *testing.T) {
	const name = "APOCI_TEST_SECRET"
	t.Setenv(name, "")
	t.Setenv(name+"_FILE", writeSecret(t, "   \n\t\n"))

	got, err := expandFileSecrets(name)
	require.Error(t, err)
	require.Contains(t, err.Error(), name+"_FILE", "error must name the var")
	require.Nil(t, got)
	require.Empty(t, os.Getenv(name), "bare var must remain unset on failure")
}

// End-to-end: registry token sourced from APOCI_REGISTRY_TOKEN_FILE.
func TestRegistryTokenFromFile(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, fmt.Sprintf("endpoint: \"https://test.example.com\"\ndataDir: %q\n", dir))

	t.Setenv("APOCI_REGISTRY_TOKEN", "")
	t.Setenv("APOCI_REGISTRY_TOKEN_FILE", writeSecret(t, "file-mounted-token\n"))

	cfg, err := Load(path, true)
	require.NoError(t, err)
	require.Equal(t, "file-mounted-token", cfg.RegistryToken)
}

// End-to-end: an unreadable _FILE fails Load instead of auto-generating.
func TestRegistryTokenFileUnreadable(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, fmt.Sprintf("endpoint: \"https://test.example.com\"\ndataDir: %q\n", dir))

	t.Setenv("APOCI_REGISTRY_TOKEN", "")
	t.Setenv("APOCI_REGISTRY_TOKEN_FILE", filepath.Join(t.TempDir(), "missing"))

	_, err := Load(path, true)
	require.Error(t, err)
}

// A file-sourced secret lands in cfg but is scrubbed from the environment, so
// spawned children (e.g. the trivy scanner) don't inherit it.
func TestFileSourcedSecretUnsetFromEnvAfterLoad(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, fmt.Sprintf("endpoint: \"https://test.example.com\"\ndataDir: %q\n", dir))

	t.Setenv("APOCI_REGISTRY_TOKEN", "")
	t.Setenv("APOCI_REGISTRY_TOKEN_FILE", writeSecret(t, "file-mounted-token\n"))

	cfg, err := Load(path, true)
	require.NoError(t, err)

	require.Equal(t, "file-mounted-token", cfg.RegistryToken, "value must live in cfg")
	require.Empty(t, os.Getenv("APOCI_REGISTRY_TOKEN"), "value must NOT remain in the environment")
}

// A directly-set bare env var is left untouched in the environment.
func TestDirectlySetSecretEnvUnchangedAfterLoad(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, fmt.Sprintf("endpoint: \"https://test.example.com\"\ndataDir: %q\n", dir))

	t.Setenv("APOCI_REGISTRY_TOKEN", "direct-value")
	require.NoError(t, os.Unsetenv("APOCI_REGISTRY_TOKEN_FILE"))

	cfg, err := Load(path, true)
	require.NoError(t, err)

	require.Equal(t, "direct-value", cfg.RegistryToken)
	require.Equal(t, "direct-value", os.Getenv("APOCI_REGISTRY_TOKEN"), "a directly-set env var must be left unchanged")
}

// Neither var set → auto-gen fallback runs, nothing written to the env.
func TestNeitherSetStillAutoGenerates(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, fmt.Sprintf("endpoint: \"https://test.example.com\"\ndataDir: %q\n", dir))

	t.Setenv("APOCI_REGISTRY_TOKEN", "")
	require.NoError(t, os.Unsetenv("APOCI_REGISTRY_TOKEN_FILE"))

	cfg, err := Load(path, true)
	require.NoError(t, err)

	require.NotEmpty(t, cfg.RegistryToken, "auto-gen fallback must still populate the token")
	require.Empty(t, os.Getenv("APOCI_REGISTRY_TOKEN"), "nothing should be written to the env")
}

// A file-mounted admin token is scrubbed from the environment after Load, like
// the other secrets.
func TestAdminTokenFromFileUnsetFromEnvAfterLoad(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, fmt.Sprintf("endpoint: \"https://test.example.com\"\ndataDir: %q\n", dir))

	t.Setenv("APOCI_ADMIN_TOKEN", "")
	t.Setenv("APOCI_ADMIN_TOKEN_FILE", writeSecret(t, "file-admin-token\n"))

	cfg, err := Load(path, true)
	require.NoError(t, err)

	require.Equal(t, "file-admin-token", cfg.AdminToken, "value must live in cfg")
	require.Empty(t, os.Getenv("APOCI_ADMIN_TOKEN"), "a file-mounted admin token must NOT remain in the environment")
}

// With no config file and mustExist=false, *_FILE expansion must still run so a
// file-supplied token is honored rather than auto-generated.
func TestFileSecretHonoredOnEnvDefaultsPath(t *testing.T) {
	dataDir := t.TempDir()
	missing := filepath.Join(t.TempDir(), "no-such-config.yaml")

	t.Setenv("APOCI_ENDPOINT", "https://test.example.com")
	t.Setenv("APOCI_DATA_DIR", dataDir)
	t.Setenv("APOCI_REGISTRY_TOKEN", "")
	t.Setenv("APOCI_REGISTRY_TOKEN_FILE", writeSecret(t, "file-mounted-token\n"))

	cfg, err := Load(missing, false)
	require.NoError(t, err, "a missing file with mustExist=false must fall back to env/defaults")

	require.Equal(t, "file-mounted-token", cfg.RegistryToken, "the *_FILE secret must be honored, not auto-generated")
	require.NotContains(t, cfg.GeneratedTokenPaths, filepath.Join(dataDir, "registry.token"),
		"a file-sourced registry token must not be flagged as generated")
}

// With mustExist=true and a missing config, Load must error before any token is
// minted and persisted to disk.
func TestMissingExplicitConfigErrorsBeforeMintingToken(t *testing.T) {
	dataDir := t.TempDir()
	missing := filepath.Join(t.TempDir(), "no-such-config.yaml")

	t.Setenv("APOCI_DATA_DIR", dataDir)
	t.Setenv("APOCI_REGISTRY_TOKEN", "")
	t.Setenv("APOCI_REGISTRY_TOKEN_FILE", writeSecret(t, "file-mounted-token\n"))

	_, err := Load(missing, true)
	require.ErrorIs(t, err, os.ErrNotExist, "an explicit missing config path must fail at the read step")

	_, statErr := os.Stat(filepath.Join(dataDir, "registry.token"))
	require.ErrorIs(t, statErr, os.ErrNotExist, "Load must bail before a token is minted")
}

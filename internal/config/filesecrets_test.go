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

	got, err := expandFileSecrets(name)
	require.NoError(t, err)
	require.Equal(t, "s3cr3t-value", os.Getenv(name))
	require.Equal(t, []string{name}, got, "a file-sourced var must be reported")
}

// Invariant 2: bare env set AND _FILE set → bare env wins, file ignored.
func TestExpandFileSecretsBareEnvWins(t *testing.T) {
	const name = "APOCI_TEST_SECRET"
	t.Setenv(name, "explicit-value")
	t.Setenv(name+"_FILE", writeSecret(t, "file-value"))

	got, err := expandFileSecrets(name)
	require.NoError(t, err)
	require.Equal(t, "explicit-value", os.Getenv(name))
	require.Empty(t, got, "a directly-set var must not be reported as file-sourced")
}

// Invariant 3: neither set → no-op, var stays empty (auto-gen fallback preserved).
func TestExpandFileSecretsNoOpWhenUnset(t *testing.T) {
	const name = "APOCI_TEST_SECRET"
	t.Setenv(name, "")
	// Ensure no _FILE is present.
	require.NoError(t, os.Unsetenv(name+"_FILE"))

	got, err := expandFileSecrets(name)
	require.NoError(t, err)
	require.Empty(t, os.Getenv(name))
	require.Empty(t, got, "no file-sourced vars when neither the bare var nor _FILE is set")
}

// Invariant 4: _FILE points at an unreadable/nonexistent path → error (loud failure).
func TestExpandFileSecretsUnreadableFileErrors(t *testing.T) {
	const name = "APOCI_TEST_SECRET"
	t.Setenv(name, "")
	t.Setenv(name+"_FILE", filepath.Join(t.TempDir(), "does-not-exist"))

	got, err := expandFileSecrets(name)
	require.Error(t, err)
	require.Nil(t, got, "no vars reported on failure")
	require.Empty(t, os.Getenv(name), "bare var must remain unset on failure")
}

// Invariant 6: _FILE points at an empty file → error (loud failure), same as the
// unreadable case. A transiently-empty mounted secret must fail startup, not fall
// through to a silently auto-generated token.
func TestExpandFileSecretsEmptyFileErrors(t *testing.T) {
	const name = "APOCI_TEST_SECRET"
	t.Setenv(name, "")
	t.Setenv(name+"_FILE", writeSecret(t, ""))

	got, err := expandFileSecrets(name)
	require.Error(t, err)
	require.Contains(t, err.Error(), name+"_FILE", "error must name the var")
	require.Nil(t, got, "no vars reported on failure")
	require.Empty(t, os.Getenv(name), "bare var must remain unset on failure")
}

// Invariant 6b: a whitespace-only file trims to empty and must fail the same way —
// TrimSpace must not turn a blank secret into a valid empty value.
func TestExpandFileSecretsWhitespaceOnlyFileErrors(t *testing.T) {
	const name = "APOCI_TEST_SECRET"
	t.Setenv(name, "")
	t.Setenv(name+"_FILE", writeSecret(t, "   \n\t\n"))

	got, err := expandFileSecrets(name)
	require.Error(t, err)
	require.Contains(t, err.Error(), name+"_FILE", "error must name the var")
	require.Nil(t, got, "no vars reported on failure")
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

// Subprocess-leak fix: a file-sourced secret must end up in the Config struct but
// NOT remain in the process environment, so it is not inherited by spawned trivy
// child processes (see internal/scanner/trivy.go, cmd.Env = append(os.Environ()...)).
func TestFileSourcedSecretUnsetFromEnvAfterLoad(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, fmt.Sprintf("endpoint: \"https://test.example.com\"\ndataDir: %q\n", dir))

	t.Setenv("APOCI_REGISTRY_TOKEN", "")
	t.Setenv("APOCI_REGISTRY_TOKEN_FILE", writeSecret(t, "file-mounted-token\n"))

	cfg, err := Load(path, true)
	require.NoError(t, err)

	require.Equal(t, "file-mounted-token", cfg.RegistryToken, "value must live in cfg")
	require.Empty(t, os.Getenv("APOCI_REGISTRY_TOKEN"), "value must NOT remain in the process environment")
}

// A secret set directly as a bare env var (not via _FILE) is pre-existing process
// state and out of scope: Load must leave it untouched in the environment.
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

// When neither the bare var nor _FILE is set, the auto-generate fallback still runs
// and nothing is spuriously written to the environment.
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

// A file-mounted APOCI_ADMIN_TOKEN — the most sensitive of the five secrets — is
// scrubbed from the environment after Load, just like the other four. The bare env
// var is never visible to cmd/apoci's remoteClient (it reads os.Getenv before Load
// ever runs expandFileSecrets), so scrubbing it breaks nothing and stops the token
// leaking into spawned trivy children.
func TestAdminTokenFromFileUnsetFromEnvAfterLoad(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, fmt.Sprintf("endpoint: \"https://test.example.com\"\ndataDir: %q\n", dir))

	t.Setenv("APOCI_ADMIN_TOKEN", "")
	t.Setenv("APOCI_ADMIN_TOKEN_FILE", writeSecret(t, "file-admin-token\n"))

	cfg, err := Load(path, true)
	require.NoError(t, err)

	require.Equal(t, "file-admin-token", cfg.AdminToken, "value must live in cfg")
	require.Empty(t, os.Getenv("APOCI_ADMIN_TOKEN"),
		"a file-mounted admin token must NOT remain in the process environment")
}

// Interaction with upstream's mustExist semantics: when there is NO config file
// and mustExist=false (the env/defaults path — a missing file is tolerated),
// expandFileSecrets must still run, so a token supplied via
// APOCI_REGISTRY_TOKEN_FILE is honored instead of a random one being minted.
// This guards against the *_FILE expansion being mis-wired to only fire on the
// config-file-present branch after the rebase.
func TestFileSecretHonoredOnEnvDefaultsPath(t *testing.T) {
	dataDir := t.TempDir()
	missing := filepath.Join(t.TempDir(), "no-such-config.yaml")

	t.Setenv("APOCI_ENDPOINT", "https://test.example.com")
	t.Setenv("APOCI_DATA_DIR", dataDir)
	t.Setenv("APOCI_REGISTRY_TOKEN", "")
	t.Setenv("APOCI_REGISTRY_TOKEN_FILE", writeSecret(t, "file-mounted-token\n"))

	cfg, err := Load(missing, false)
	require.NoError(t, err, "a missing file with mustExist=false must fall back to env/defaults")

	require.Equal(t, "file-mounted-token", cfg.RegistryToken,
		"the *_FILE secret must be honored on the env/defaults path, not auto-generated")
	require.NotContains(t, cfg.GeneratedTokenPaths, filepath.Join(dataDir, "registry.token"),
		"a file-sourced registry token must not be flagged as generated")
}

// Ordering guarantee across the mustExist × auto-gen boundary: when mustExist=true
// and the explicit config path is missing, Load must error at the config-read step
// BEFORE expandFileSecrets/applyTokenDefaults run — so no token is minted and
// persisted to disk. A valid APOCI_REGISTRY_TOKEN_FILE is present precisely to prove
// the *_FILE/auto-gen machinery never got the chance to run.
func TestMissingExplicitConfigErrorsBeforeMintingToken(t *testing.T) {
	dataDir := t.TempDir()
	missing := filepath.Join(t.TempDir(), "no-such-config.yaml")

	t.Setenv("APOCI_DATA_DIR", dataDir)
	t.Setenv("APOCI_REGISTRY_TOKEN", "")
	t.Setenv("APOCI_REGISTRY_TOKEN_FILE", writeSecret(t, "file-mounted-token\n"))

	_, err := Load(missing, true)
	require.ErrorIs(t, err, os.ErrNotExist,
		"an explicit missing config path must fail at the read step")

	_, statErr := os.Stat(filepath.Join(dataDir, "registry.token"))
	require.ErrorIs(t, statErr, os.ErrNotExist,
		"Load must bail before applyTokenDefaults mints and persists a token")
}

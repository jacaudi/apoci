package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/config"
)

func TestWarnGeneratedTokensEmitsOnePerPath(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	cfg := &config.Config{
		GeneratedTokenPaths: []string{
			"/data/registry.token",
			"/data/admin.token",
		},
	}

	warnGeneratedTokens(logger, cfg)

	out := buf.String()
	if got := strings.Count(out, "level=WARN"); got != 2 {
		t.Fatalf("expected 2 warnings, got %d in output:\n%s", got, out)
	}
	if !strings.Contains(out, "/data/registry.token") {
		t.Errorf("warning should name the registry token path; got:\n%s", out)
	}
	if !strings.Contains(out, "/data/admin.token") {
		t.Errorf("warning should name the admin token path; got:\n%s", out)
	}
}

func TestWarnGeneratedTokensSilentWhenNoneGenerated(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	cfg := &config.Config{
		RegistryToken: "operator-supplied",
	}

	warnGeneratedTokens(logger, cfg)

	if buf.Len() != 0 {
		t.Errorf("no tokens were generated, expected no output; got:\n%s", buf.String())
	}
}

func TestWarnGeneratedTokensNeverLogsTokenValue(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	const secret = "deadbeefcafef00d"
	cfg := &config.Config{
		RegistryToken:       secret,
		GeneratedTokenPaths: []string{"/data/registry.token"},
	}

	warnGeneratedTokens(logger, cfg)

	if strings.Contains(buf.String(), secret) {
		t.Errorf("warning must not leak the token value; got:\n%s", buf.String())
	}
}

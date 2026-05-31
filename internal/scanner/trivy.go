package scanner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
)

// TrivyConfig configures the Trivy adapter.
type TrivyConfig struct {
	BinaryPath string
	Insecure   bool
	// Username/Password authenticate against the apoci registry. apoci accepts
	// the registry token as the basic-auth password.
	Username string
	Password string
}

// Trivy scans images by shelling out to the trivy binary, pointing it at the
// apoci registry by digest.
type Trivy struct {
	cfg    TrivyConfig
	logger *slog.Logger
}

func NewTrivy(cfg TrivyConfig, logger *slog.Logger) *Trivy {
	if cfg.BinaryPath == "" {
		cfg.BinaryPath = "trivy"
	}
	return &Trivy{cfg: cfg, logger: logger}
}

func (t *Trivy) Name() string { return "trivy" }

func (t *Trivy) Scan(ctx context.Context, imageRef string) (Report, error) {
	args := []string{"image", "--quiet", "--no-progress", "--format", "json", "--scanners", "vuln", imageRef}

	cmd := exec.CommandContext(ctx, t.cfg.BinaryPath, args...) //nolint:gosec // G204: operator-configured binary, validated args
	cmd.Env = append(os.Environ(),
		"TRIVY_USERNAME="+t.cfg.Username,
		"TRIVY_PASSWORD="+t.cfg.Password,
	)
	if t.cfg.Insecure {
		cmd.Env = append(cmd.Env, "TRIVY_INSECURE=true")
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return Report{}, fmt.Errorf("running trivy: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	raw := stdout.Bytes()
	summary, err := parseTrivySummary(raw)
	if err != nil {
		return Report{}, fmt.Errorf("parsing trivy output: %w", err)
	}
	return Report{Raw: raw, MediaType: ReportMediaType, Summary: summary}, nil
}

// parseTrivySummary tallies vulnerabilities by severity from Trivy JSON output.
func parseTrivySummary(raw []byte) (Summary, error) {
	var out struct {
		Results []struct {
			Vulnerabilities []struct {
				Severity string `json:"Severity"`
			} `json:"Vulnerabilities"`
		} `json:"Results"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Summary{}, err
	}

	var s Summary
	for _, r := range out.Results {
		for _, v := range r.Vulnerabilities {
			switch strings.ToUpper(v.Severity) {
			case "CRITICAL":
				s.Critical++
			case "HIGH":
				s.High++
			case "MEDIUM":
				s.Medium++
			case "LOW":
				s.Low++
			default:
				s.Unknown++
			}
		}
	}
	return s, nil
}

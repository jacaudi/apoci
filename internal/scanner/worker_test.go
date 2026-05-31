package scanner

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

type fakeScanner struct {
	report Report
	err    error
	calls  int
}

func (f *fakeScanner) Name() string { return "fake" }
func (f *fakeScanner) Scan(_ context.Context, _ string) (Report, error) {
	f.calls++
	return f.report, f.err
}

type fakeRegistry struct {
	mu        sync.Mutex
	has       bool
	attached  []attachCall
	attachErr error
}

type attachCall struct {
	repo, subject, artifactType string
	annotations                 map[string]string
	payload                     []byte
}

func (r *fakeRegistry) HasReferrer(_ context.Context, _, _, _ string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.has, nil
}

func (r *fakeRegistry) AttachReferrer(_ context.Context, repo, subject, artifactType string, annotations map[string]string, payload []byte, _ string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.attachErr != nil {
		return "", r.attachErr
	}
	r.attached = append(r.attached, attachCall{repo, subject, artifactType, annotations, payload})
	return "sha256:ref", nil
}

func newTestWorker(s Scanner, reg Registry) *Worker {
	return NewWorker(Config{
		Scanner:   s,
		Registry:  reg,
		Host:      "reg.test",
		QueueSize: 10,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestOnManifestPushedFilters(t *testing.T) {
	w := newTestWorker(&fakeScanner{}, &fakeRegistry{})

	// Referrer (subject set) -> skipped.
	subj := "sha256:subject"
	w.OnManifestPushed("repo", "v1", "sha256:a", "application/vnd.oci.image.manifest.v1+json", &subj)
	require.Equal(t, 0, w.queue.Len())

	// Non-image media type -> skipped.
	w.OnManifestPushed("repo", "v1", "sha256:b", "application/vnd.oci.image.config.v1+json", nil)
	require.Equal(t, 0, w.queue.Len())

	// Image manifest -> enqueued.
	w.OnManifestPushed("repo", "v1", "sha256:c", "application/vnd.oci.image.manifest.v1+json", nil)
	require.Equal(t, 1, w.queue.Len())
}

func TestProcessAttachesReport(t *testing.T) {
	fs := &fakeScanner{report: Report{
		Raw:       []byte(`{}`),
		MediaType: ReportMediaType,
		Summary:   Summary{Critical: 2, High: 1},
	}}
	reg := &fakeRegistry{}
	w := newTestWorker(fs, reg)

	w.process(context.Background(), job{repo: "repo", digest: "sha256:img"})

	require.Equal(t, 1, fs.calls)
	require.Len(t, reg.attached, 1)
	got := reg.attached[0]
	require.Equal(t, "repo", got.repo)
	require.Equal(t, "sha256:img", got.subject)
	require.Equal(t, ArtifactType, got.artifactType)
	require.Equal(t, "2", got.annotations[AnnCritical])
	require.Equal(t, "1", got.annotations[AnnHigh])
}

func TestProcessSkipsWhenReferrerExists(t *testing.T) {
	fs := &fakeScanner{}
	reg := &fakeRegistry{has: true}
	w := newTestWorker(fs, reg)

	w.process(context.Background(), job{repo: "repo", digest: "sha256:img"})

	require.Equal(t, 0, fs.calls, "scanner should not run when report already attached")
	require.Empty(t, reg.attached)
}

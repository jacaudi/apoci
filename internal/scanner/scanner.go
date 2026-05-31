// Package scanner provides inline vulnerability scanning of pushed images,
// attaching each report as an OCI referrer so it federates to followers.
package scanner

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"codeberg.org/gruf/go-runners"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/queue"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/util"
)

const (
	// ArtifactType marks a manifest as an apoci vulnerability report.
	ArtifactType = "application/vnd.apoci.cve-report.v1"
	// ReportMediaType is the media type of the raw scanner output layer.
	ReportMediaType = "application/json"

	AnnCreated  = "dev.apoci.scan.created"
	AnnScanner  = "dev.apoci.scan.scanner"
	AnnCritical = "dev.apoci.scan.critical"
	AnnHigh     = "dev.apoci.scan.high"
	AnnMedium   = "dev.apoci.scan.medium"
	AnnLow      = "dev.apoci.scan.low"
	AnnUnknown  = "dev.apoci.scan.unknown"
)

// Summary is the per-severity vulnerability count from a scan.
type Summary struct {
	Critical int
	High     int
	Medium   int
	Low      int
	Unknown  int
}

// Report is the result of scanning one image reference.
type Report struct {
	Raw       []byte // raw scanner output, stored as the referrer layer
	MediaType string
	Summary   Summary
}

// Scanner runs a vulnerability scan against an image reference and returns the
// raw report plus a severity summary.
type Scanner interface {
	Name() string
	Scan(ctx context.Context, imageRef string) (Report, error)
}

// Registry is the subset of the OCI registry the worker needs to store and
// dedupe scan referrers.
type Registry interface {
	AttachReferrer(ctx context.Context, repo, subjectDigest, artifactType string, annotations map[string]string, payload []byte, payloadMediaType string) (string, error)
	HasReferrer(ctx context.Context, repo, subjectDigest, artifactType string) (bool, error)
}

type job struct {
	repo   string
	digest string
}

// Config configures a Worker.
type Config struct {
	Scanner   Scanner
	Registry  Registry
	Host      string // registry host (no scheme) used to build scan image refs
	Timeout   time.Duration
	QueueSize int
}

// Worker scans pushed image manifests off the push path. It implements
// oci.ManifestObserver and workers.Service.
type Worker struct {
	scanner  Scanner
	reg      Registry
	host     string
	timeout  time.Duration
	maxQueue int
	queue    *queue.SimpleQueue[job]
	logger   *slog.Logger
	service  runners.Service
}

func NewWorker(cfg Config, logger *slog.Logger) *Worker {
	return &Worker{
		scanner:  cfg.Scanner,
		reg:      cfg.Registry,
		host:     cfg.Host,
		timeout:  cfg.Timeout,
		maxQueue: cfg.QueueSize,
		queue:    queue.NewSimpleQueue[job](cfg.QueueSize),
		logger:   logger.With("component", "scanner", "scanner", cfg.Scanner.Name()),
	}
}

// scannableMediaTypes are the manifest types worth scanning. Indexes are
// included; Trivy resolves them to per-platform images.
var scannableMediaTypes = map[string]bool{
	ocispec.MediaTypeImageManifest:                              true,
	ocispec.MediaTypeImageIndex:                                 true,
	"application/vnd.docker.distribution.manifest.v2+json":      true,
	"application/vnd.docker.distribution.manifest.list.v2+json": true,
}

// OnManifestPushed enqueues image manifests for scanning. Referrers
// (subjectDigest != nil) and non-image media types are skipped. tag is unused;
// scanning keys on the digest.
func (w *Worker) OnManifestPushed(repo, _, digest, mediaType string, subjectDigest *string) {
	if subjectDigest != nil || !scannableMediaTypes[mediaType] {
		return
	}
	if !w.queue.TryPush(job{repo: repo, digest: digest}, w.maxQueue) {
		w.logger.Warn("scan queue full, dropping scan", "repo", repo, "digest", digest)
	}
}

func (w *Worker) Start(ctx context.Context) {
	w.service.GoRun(func(svcCtx context.Context) {
		util.Must(w.logger, func() {
			w.run(ctx, svcCtx)
		})
	})
}

func (w *Worker) Stop() {
	w.service.Stop()
}

func (w *Worker) run(parentCtx, svcCtx context.Context) {
	for {
		j, ok := w.queue.PopCtx(svcCtx)
		if !ok {
			return
		}
		w.process(parentCtx, j)
	}
}

func (w *Worker) process(ctx context.Context, j job) {
	exists, err := w.reg.HasReferrer(ctx, j.repo, j.digest, ArtifactType)
	if err != nil {
		w.logger.Warn("scan: referrer check failed", "repo", j.repo, "digest", j.digest, "error", err)
	} else if exists {
		w.logger.Debug("scan: report already attached, skipping", "repo", j.repo, "digest", j.digest)
		return
	}

	scanCtx := ctx
	if w.timeout > 0 {
		var cancel context.CancelFunc
		scanCtx, cancel = context.WithTimeout(ctx, w.timeout)
		defer cancel()
	}

	ref := fmt.Sprintf("%s/%s@%s", w.host, j.repo, j.digest)
	report, err := w.scanner.Scan(scanCtx, ref)
	if err != nil {
		w.logger.Error("scan failed", "ref", ref, "error", err)
		return
	}

	annotations := map[string]string{
		AnnCreated:  time.Now().UTC().Format(time.RFC3339),
		AnnScanner:  w.scanner.Name(),
		AnnCritical: strconv.Itoa(report.Summary.Critical),
		AnnHigh:     strconv.Itoa(report.Summary.High),
		AnnMedium:   strconv.Itoa(report.Summary.Medium),
		AnnLow:      strconv.Itoa(report.Summary.Low),
		AnnUnknown:  strconv.Itoa(report.Summary.Unknown),
	}

	mediaType := report.MediaType
	if mediaType == "" {
		mediaType = ReportMediaType
	}
	refDigest, err := w.reg.AttachReferrer(ctx, j.repo, j.digest, ArtifactType, annotations, report.Raw, mediaType)
	if err != nil {
		w.logger.Error("scan: attaching report referrer failed", "repo", j.repo, "digest", j.digest, "error", err)
		return
	}
	w.logger.Info("scan report attached",
		"repo", j.repo,
		"digest", j.digest,
		"referrer", refDigest,
		"critical", report.Summary.Critical,
		"high", report.Summary.High,
	)
}

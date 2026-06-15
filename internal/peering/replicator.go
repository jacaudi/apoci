package peering

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"runtime/debug"
	"sync"
	"time"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/blobstore"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/metrics"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/notify"
)

// replicationMaxConcurrency returns the max concurrent replications based on GOMAXPROCS.
// Uses 10x multiplier since replication is I/O bound (HTTP fetch + disk write).
func replicationMaxConcurrency() int {
	n := runtime.GOMAXPROCS(0) * 10
	if n < 10 {
		return 10
	}
	return n
}

type ReplicatorRepository interface {
	FindRepoForBlob(ctx context.Context, digest string) (string, error)
	PutBlob(ctx context.Context, digest string, sizeBytes int64, mediaType *string, storedLocally bool) error
}

type BlobStreamFetcher interface {
	FetchBlobStream(ctx context.Context, peerEndpoint, repo, digest string) (*BlobStream, error)
	FetchStream(ctx context.Context, sourceURL string) (*BlobStream, error)
}

type BlobReplicator struct {
	db             ReplicatorRepository
	blobs          blobstore.BlobStore
	fetcher        BlobStreamFetcher
	notifier       Notifier
	logger         *slog.Logger
	maxConcurrency int
	sem            chan struct{}
	wg             sync.WaitGroup
	shutdownCtx    context.Context
	cancel         context.CancelFunc
}

func NewBlobReplicator(db ReplicatorRepository, blobs blobstore.BlobStore, fetcher BlobStreamFetcher, notifier Notifier, logger *slog.Logger) *BlobReplicator {
	maxConcurrency := replicationMaxConcurrency()
	shutdownCtx, cancel := context.WithCancel(context.Background())
	return &BlobReplicator{
		shutdownCtx:    shutdownCtx,
		cancel:         cancel,
		db:             db,
		blobs:          blobs,
		fetcher:        fetcher,
		notifier:       notifier,
		logger:         logger,
		maxConcurrency: maxConcurrency,
		sem:            make(chan struct{}, maxConcurrency),
	}
}

// ReplicateBlob fetches a blob from a peer and stores it locally in the background.
// The repo is derived from manifest layer references in the database; if none exist
// yet, replication is skipped and the blob will be fetched on-demand via pull-through.
func (r *BlobReplicator) ReplicateBlob(ctx context.Context, peerEndpoint, digest string, size int64) {
	if exists, err := r.blobs.Exists(ctx, digest); err != nil {
		r.logger.Warn("failed to check blob existence before replication", "digest", digest, "error", err)
		return
	} else if exists {
		return
	}

	r.submit(digest, func(bgCtx context.Context) {
		r.replicateBlob(bgCtx, peerEndpoint, digest)
	})
}

// ReplicateFromURL eagerly fetches a content-addressed blob from an arbitrary
// URL (used by package backends whose download routes don't follow the OCI
// /v2/repo/blobs/digest layout). On success the blob is stored locally and
// the blobs row is marked stored_locally=true. Errors are logged, not
// returned: replication is best-effort, and the redirect-to-peer fallback
// still works if this fails.
func (r *BlobReplicator) ReplicateFromURL(ctx context.Context, sourceURL, digest string) {
	if exists, err := r.blobs.Exists(ctx, digest); err != nil {
		r.logger.Warn("failed to check blob existence before replication", "digest", digest, "error", err)
		return
	} else if exists {
		return
	}

	r.submit(digest, func(bgCtx context.Context) {
		r.replicateFromURL(bgCtx, sourceURL, digest)
	})
}

// submit bounds replication by acquiring the concurrency semaphore before
// spawning the goroutine, so a flood of peer announces cannot create an
// unbounded number of goroutines parked on the semaphore. Work runs under a
// shutdown-cancelable context with a per-blob timeout; saturated submissions
// are dropped (replication is best-effort and falls back to pull-through).
func (r *BlobReplicator) submit(digest string, work func(ctx context.Context)) {
	metrics.BlobReplicationsStarted.Inc()

	select {
	case r.sem <- struct{}{}:
	default:
		metrics.BlobReplicationsFailed.Add(1)
		r.logger.Warn("replication concurrency saturated, dropping blob", "digest", digest)
		return
	}

	metrics.BlobReplicationsInFlight.Inc()
	r.wg.Go(func() {
		defer func() { <-r.sem }()
		defer metrics.BlobReplicationsInFlight.Dec()
		defer r.recoverPanic(digest)

		bgCtx, cancel := context.WithTimeout(r.shutdownCtx, 5*time.Minute)
		defer cancel()
		work(bgCtx)
	})
}

func (r *BlobReplicator) replicateFromURL(ctx context.Context, sourceURL, digest string) {
	fetchStart := time.Now()
	stream, err := r.fetcher.FetchStream(ctx, sourceURL)
	if err != nil {
		metrics.BlobReplicationsFailed.Add(1)
		r.logger.Warn("eager replication failed", "digest", digest, "url", sourceURL, "error", err)
		r.notifier.Send(notify.EventReplicationFailure, fmt.Sprintf("Failed to replicate blob %s from %s: %v", digest, sourceURL, err))
		return
	}
	defer func() {
		if err := stream.Body.Close(); err != nil {
			r.logger.Warn("failed to close blob stream", "digest", digest, "error", err)
		}
	}()

	storedDigest, size, err := r.blobs.Put(ctx, stream.Body, digest)
	if err != nil {
		metrics.BlobReplicationsFailed.Add(1)
		r.logger.Warn("failed to store replicated blob", "digest", digest, "error", err)
		r.notifier.Send(notify.EventReplicationFailure, fmt.Sprintf("Failed to store replicated blob %s: %v", digest, err))
		return
	}

	mt := "application/octet-stream"
	if err := r.db.PutBlob(ctx, storedDigest, size, &mt, true); err != nil {
		r.logger.Warn("failed to update blob metadata after replication", "digest", digest, "error", err)
		return
	}

	metrics.BlobFetchDuration.Observe(time.Since(fetchStart).Seconds())
	metrics.BlobReplicationsSucceeded.Add(1)
	r.logger.Info("eagerly replicated blob", "digest", digest, "url", sourceURL, "size", size)
}

// recoverPanic recovers from panics in replication goroutines and logs them.
func (r *BlobReplicator) recoverPanic(digest string) {
	if rec := recover(); rec != nil {
		r.logger.Error("replication panic recovered",
			"digest", digest,
			"panic", rec,
			"stack", string(debug.Stack()),
		)
	}
}

// Wait blocks until all in-flight replications complete, then releases the
// shutdown context. The semaphore caps the in-flight set and excess work is
// dropped rather than queued, so this drains in bounded time (the per-blob
// timeout), and the overall shutdown deadline is enforced by Workers.Stop.
func (r *BlobReplicator) Wait() {
	r.wg.Wait()
	r.cancel()
}

func (r *BlobReplicator) replicateBlob(ctx context.Context, peerEndpoint, digest string) {
	// Find a repo that references this blob so we can construct the OCI pull URL.
	// Blobs are served under /v2/{repo}/blobs/{digest}, so we need a valid repo name.
	repo, _ := r.db.FindRepoForBlob(ctx, digest)
	if repo == "" {
		// No repo references this blob yet (blob announce arrived before the manifest).
		// Skip eager replication; the blob will be fetched on-demand via pull-through
		// when a client actually requests it.
		r.logger.Debug("skipping eager replication: no repo references blob yet", "digest", digest)
		return
	}

	fetchStart := time.Now()
	stream, err := r.fetcher.FetchBlobStream(ctx, peerEndpoint, repo, digest)
	if err != nil {
		metrics.BlobReplicationsFailed.Add(1)
		r.logger.Warn("eager replication failed",
			"digest", digest,
			"peer", peerEndpoint,
			"error", err,
		)
		r.notifier.Send(notify.EventReplicationFailure, fmt.Sprintf("Failed to replicate blob %s from %s: %v", digest, peerEndpoint, err))
		return
	}
	defer func() {
		if err := stream.Body.Close(); err != nil {
			r.logger.Warn("failed to close blob stream", "digest", digest, "error", err)
		}
	}()

	storedDigest, size, err := r.blobs.Put(ctx, stream.Body, digest)
	if err != nil {
		metrics.BlobReplicationsFailed.Add(1)
		r.logger.Warn("failed to store replicated blob",
			"digest", digest,
			"error", err,
		)
		r.notifier.Send(notify.EventReplicationFailure, fmt.Sprintf("Failed to store replicated blob %s: %v", digest, err))
		return
	}

	mt := "application/octet-stream"
	if err := r.db.PutBlob(ctx, storedDigest, size, &mt, true); err != nil {
		r.logger.Warn("failed to update blob metadata after replication",
			"digest", digest,
			"error", err,
		)
		return
	}

	metrics.BlobFetchDuration.Observe(time.Since(fetchStart).Seconds())
	metrics.BlobReplicationsSucceeded.Add(1)
	r.logger.Info("eagerly replicated blob",
		"digest", digest,
		"peer", peerEndpoint,
		"size", size,
	)
}

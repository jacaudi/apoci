package peering

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"sync"
	"time"

	"codeberg.org/gruf/go-runners"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/blobstore"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/metrics"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/notify"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/util"
)

type GCRepository interface {
	CleanupStalePeerBlobs(ctx context.Context, olderThan time.Duration) (int64, error)
	OrphanedBlobs(ctx context.Context, limit int) ([]string, error)
	DeleteBlob(ctx context.Context, digest string) error
	AllBlobDigests(ctx context.Context, pageSize int) (map[string]bool, error)
	PruneDeletedManifests(ctx context.Context, olderThan time.Duration) (int64, error)

	// Retention + untagged-manifest prune.
	ListOCIPackagesForRetention(ctx context.Context, startAfter string, limit int) ([]database.PackageRetention, error)
	ListTagsForRetention(ctx context.Context, packageID int64) ([]database.TagForRetention, error)
	DeletePackageTag(ctx context.Context, packageID int64, name string) error
	PruneUntaggedManifests(ctx context.Context, olderThan time.Duration, limit int) ([]database.UntaggedManifest, error)
}

type FederationPublisher interface {
	PublishTagDelete(ctx context.Context, repo, tag string) error
	PublishManifestDelete(ctx context.Context, repo, digest string) error
}

// RetentionPolicy applies to OCI packages. Zero KeepLastN/MaxAge disables that knob.
// KeepLastN counts only mutable, non-pinned tags — pinned and immutable tags are
// always kept and don't consume a slot.
type RetentionPolicy struct {
	KeepLastN   int
	MaxAge      time.Duration
	PinnedGlobs []string
}

type Notifier interface {
	Send(event, message string)
}

type GCConfig struct {
	Interval            time.Duration
	StalePeerBlobAge    time.Duration
	OrphanBatchSize     int
	BlobGCGracePeriod   time.Duration
	UntaggedManifestAge time.Duration
	UntaggedBatchSize   int
	RetentionDefaults   RetentionPolicy
	// Cap per-cycle retention work to avoid hogging a single GC run on a big instance.
	RetentionTagsPerCycle int
}

type GarbageCollector struct {
	cfg       GCConfig
	db        GCRepository
	blobs     blobstore.BlobStore
	publisher FederationPublisher // optional; nil = no federation of GC-driven deletes
	notifier  Notifier
	logger    *slog.Logger
	service   runners.Service
	mu        sync.Mutex
}

func NewGarbageCollector(cfg GCConfig, db GCRepository, blobs blobstore.BlobStore, notifier Notifier, logger *slog.Logger) *GarbageCollector {
	return &GarbageCollector{
		cfg:      cfg,
		db:       db,
		blobs:    blobs,
		notifier: notifier,
		logger:   logger,
	}
}

func (gc *GarbageCollector) SetFederationPublisher(p FederationPublisher) {
	gc.publisher = p
}

func (gc *GarbageCollector) Start(ctx context.Context) {
	gc.service.GoRun(func(svcCtx context.Context) {
		util.Must(gc.logger, func() {
			gc.run(ctx, svcCtx)
		})
	})
}

func (gc *GarbageCollector) Stop() {
	gc.service.Stop()
}

func (gc *GarbageCollector) RunOnce(ctx context.Context) {
	gc.collect(ctx)
}

func (gc *GarbageCollector) run(parentCtx, svcCtx context.Context) {
	// Run once shortly after startup.
	timer := time.NewTimer(time.Minute)
	defer timer.Stop()

	for {
		select {
		case <-svcCtx.Done():
			return
		case <-parentCtx.Done():
			return
		case <-timer.C:
			gc.collect(parentCtx)
			timer.Reset(gc.cfg.Interval)
		}
	}
}

const deletedManifestRetention = 30 * 24 * time.Hour

func (gc *GarbageCollector) collect(ctx context.Context) {
	gc.mu.Lock()
	defer gc.mu.Unlock()

	gc.logger.Info("starting garbage collection cycle")

	gc.retentionSweep(ctx)
	gc.pruneUntaggedManifests(ctx)
	gc.cleanupStalePeerBlobs(ctx)
	gc.cleanupOrphanedBlobMetadata(ctx)
	gc.cleanupOrphanedBlobFiles(ctx)
	gc.pruneDeletedManifests(ctx)

	metrics.GCCyclesCompleted.Add(1)
	gc.logger.Info("garbage collection cycle complete")
}

// effectiveRetention resolves per-package overrides against the global default.
// nil = inherit; non-nil zero = explicitly disabled for this package.
func (gc *GarbageCollector) effectiveRetention(pkg database.PackageRetention) RetentionPolicy {
	out := gc.cfg.RetentionDefaults
	if pkg.RetentionKeepLast != nil {
		out.KeepLastN = *pkg.RetentionKeepLast
	}
	if pkg.RetentionMaxAgeSeconds != nil {
		out.MaxAge = time.Duration(*pkg.RetentionMaxAgeSeconds) * time.Second
	}
	if pkg.RetentionPinnedGlobs != nil {
		raw := strings.TrimSpace(*pkg.RetentionPinnedGlobs)
		if raw == "" {
			out.PinnedGlobs = nil
		} else {
			parts := strings.Split(raw, ",")
			out.PinnedGlobs = make([]string, 0, len(parts))
			for _, p := range parts {
				if g := strings.TrimSpace(p); g != "" {
					out.PinnedGlobs = append(out.PinnedGlobs, g)
				}
			}
		}
	}
	return out
}

func tagPinned(name string, globs []string) bool {
	for _, g := range globs {
		if ok, err := path.Match(g, name); err == nil && ok {
			return true
		}
	}
	return false
}

// retentionSweep deletes tags exceeding KeepLastN or older than MaxAge, then
// federates each delete. Pinned and immutable tags are always kept.
func (gc *GarbageCollector) retentionSweep(ctx context.Context) {
	budget := gc.cfg.RetentionTagsPerCycle
	if budget <= 0 {
		budget = 10000
	}

	totalDeleted := 0
	startAfter := ""
	for totalDeleted < budget {
		pkgs, err := gc.db.ListOCIPackagesForRetention(ctx, startAfter, 100)
		if err != nil {
			gc.logger.Error("gc: retention: list packages failed", "error", err)
			gc.notifier.Send(notify.EventGCError, fmt.Sprintf("GC: retention list packages failed: %v", err))
			return
		}
		if len(pkgs) == 0 {
			break
		}
		for _, pkg := range pkgs {
			if totalDeleted >= budget {
				break
			}
			policy := gc.effectiveRetention(pkg)
			if policy.KeepLastN <= 0 && policy.MaxAge <= 0 {
				continue
			}
			deleted := gc.applyRetentionToPackage(ctx, pkg, policy, budget-totalDeleted)
			totalDeleted += deleted
		}
		startAfter = pkgs[len(pkgs)-1].Name
		if len(pkgs) < 100 {
			break
		}
	}

	if totalDeleted > 0 {
		metrics.GCRetentionTagsDeleted.Add(float64(totalDeleted))
		gc.logger.Info("gc: retention deleted tags", "count", totalDeleted)
	}
}

func (gc *GarbageCollector) applyRetentionToPackage(ctx context.Context, pkg database.PackageRetention, policy RetentionPolicy, budget int) int {
	tags, err := gc.db.ListTagsForRetention(ctx, pkg.ID)
	if err != nil {
		gc.logger.Warn("gc: retention: list tags failed", "package", pkg.Name, "error", err)
		return 0
	}

	cutoff := time.Now().Add(-policy.MaxAge)
	candidates := make([]database.TagForRetention, 0, len(tags))
	kept := 0
	for _, t := range tags {
		if t.Immutable || tagPinned(t.Name, policy.PinnedGlobs) {
			continue
		}
		drop := false
		if policy.KeepLastN > 0 {
			if kept >= policy.KeepLastN {
				drop = true
			}
		}
		if !drop && policy.MaxAge > 0 && t.UpdatedAt.Before(cutoff) {
			drop = true
		}
		if drop {
			candidates = append(candidates, t)
		} else if policy.KeepLastN > 0 {
			kept++
		}
	}

	deleted := 0
	for _, t := range candidates {
		if deleted >= budget {
			break
		}
		if err := gc.db.DeletePackageTag(ctx, pkg.ID, t.Name); err != nil {
			gc.logger.Warn("gc: retention: delete tag failed", "package", pkg.Name, "tag", t.Name, "error", err)
			continue
		}
		if gc.publisher != nil {
			if err := gc.publisher.PublishTagDelete(ctx, pkg.Name, t.Name); err != nil {
				gc.logger.Warn("gc: retention: federate tag delete failed", "package", pkg.Name, "tag", t.Name, "error", err)
			}
		}
		deleted++
	}
	return deleted
}

func (gc *GarbageCollector) pruneUntaggedManifests(ctx context.Context) {
	if gc.cfg.UntaggedManifestAge <= 0 {
		return
	}
	limit := gc.cfg.UntaggedBatchSize
	if limit <= 0 {
		limit = 500
	}

	rows, err := gc.db.PruneUntaggedManifests(ctx, gc.cfg.UntaggedManifestAge, limit)
	if err != nil {
		gc.logger.Error("gc: prune untagged manifests failed", "error", err)
		gc.notifier.Send(notify.EventGCError, fmt.Sprintf("GC: prune untagged manifests failed: %v", err))
		return
	}
	if len(rows) == 0 {
		return
	}
	metrics.GCUntaggedManifestsPruned.Add(float64(len(rows)))
	gc.logger.Info("gc: pruned untagged manifests", "count", len(rows))

	if gc.publisher != nil {
		for _, r := range rows {
			if err := gc.publisher.PublishManifestDelete(ctx, r.PackageName, r.Digest); err != nil {
				gc.logger.Warn("gc: federate manifest delete failed", "repo", r.PackageName, "digest", r.Digest, "error", err)
			}
		}
	}
}

func (gc *GarbageCollector) pruneDeletedManifests(ctx context.Context) {
	n, err := gc.db.PruneDeletedManifests(ctx, deletedManifestRetention)
	if err != nil {
		gc.logger.Error("gc: failed to prune deleted manifest tombstones", "error", err)
		return
	}
	if n > 0 {
		gc.logger.Info("gc: pruned old manifest tombstones", "count", n)
	}
}

// cleanupStalePeerBlobs removes peer blob references not verified in 30 days.
func (gc *GarbageCollector) cleanupStalePeerBlobs(ctx context.Context) {
	n, err := gc.db.CleanupStalePeerBlobs(ctx, gc.cfg.StalePeerBlobAge)
	if err != nil {
		gc.logger.Error("gc: failed to cleanup stale peer blobs", "error", err)
		gc.notifier.Send(notify.EventGCError, fmt.Sprintf("GC: failed to cleanup stale peer blobs: %v", err))
		return
	}
	if n > 0 {
		metrics.GCStalePeerBlobs.Add(float64(n))
		gc.logger.Info("gc: removed stale peer blob references", "count", n)
	}
}

// cleanupOrphanedBlobMetadata removes blob DB records that are not stored locally
// and have no peer references or manifest layer references.
func (gc *GarbageCollector) cleanupOrphanedBlobMetadata(ctx context.Context) {
	digests, err := gc.db.OrphanedBlobs(ctx, gc.cfg.OrphanBatchSize)
	if err != nil {
		gc.logger.Error("gc: failed to find orphaned blobs", "error", err)
		gc.notifier.Send(notify.EventGCError, fmt.Sprintf("GC: failed to find orphaned blobs: %v", err))
		return
	}

	removed := 0
	for _, digest := range digests {
		if err := gc.db.DeleteBlob(ctx, digest); err != nil {
			gc.logger.Warn("gc: failed to delete orphaned blob metadata", "digest", digest, "error", err)
			continue
		}
		removed++
	}

	if removed > 0 {
		metrics.GCOrphanedMetadata.Add(float64(removed))
		gc.logger.Info("gc: removed orphaned blob metadata", "count", removed)
	}
}

// cleanupOrphanedBlobFiles removes blob files on disk that have no DB record.
// Files modified within BlobGCGracePeriod are skipped to dodge the upload race
// (disk write landed, DB row not yet inserted).
func (gc *GarbageCollector) cleanupOrphanedBlobFiles(ctx context.Context) {
	diskDigests, err := gc.blobs.ListDigests(ctx)
	if err != nil {
		gc.logger.Error("gc: failed to list blob files on disk", "error", err)
		gc.notifier.Send(notify.EventGCError, fmt.Sprintf("GC: failed to list blob files on disk: %v", err))
		return
	}

	knownDigests, err := gc.db.AllBlobDigests(ctx, 1000)
	if err != nil {
		gc.logger.Error("gc: failed to list known blob digests", "error", err)
		gc.notifier.Send(notify.EventGCError, fmt.Sprintf("GC: failed to list known blob digests: %v", err))
		return
	}

	graceCutoff := time.Now().Add(-gc.cfg.BlobGCGracePeriod)
	removed := 0
	for _, digest := range diskDigests {
		if knownDigests[digest] {
			continue
		}
		if gc.cfg.BlobGCGracePeriod > 0 {
			mtime, err := gc.blobs.ModTime(ctx, digest)
			if err != nil {
				if !errors.Is(err, blobstore.ErrBlobNotFound) {
					gc.logger.Warn("gc: failed to stat blob file", "digest", digest, "error", err)
				}
				continue
			}
			if mtime.After(graceCutoff) {
				continue
			}
		}
		if err := gc.blobs.Delete(ctx, digest); err != nil {
			gc.logger.Warn("gc: failed to delete orphaned blob file", "digest", digest, "error", err)
			continue
		}
		removed++
	}

	if removed > 0 {
		metrics.GCOrphanedFiles.Add(float64(removed))
		gc.logger.Info("gc: removed orphaned blob files from disk", "count", removed)
	}
}

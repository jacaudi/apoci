package peering

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"sync"
	"sync/atomic"
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
	OrphanedBlobs(ctx context.Context, limit int, createdBefore time.Time) ([]string, error)
	DeleteBlob(ctx context.Context, digest string) error
	AllBlobDigests(ctx context.Context, pageSize int) (map[string]bool, error)
	PruneDeletedManifests(ctx context.Context, olderThan time.Duration) (int64, error)
	ListBlobsForReconcile(ctx context.Context, startAfter string, limit int) ([]database.BlobReconcileRow, error)
	HasPeerBlob(ctx context.Context, digest string) (bool, error)
	SetBlobStoredLocally(ctx context.Context, digest string, stored bool) error
	IsBlobReferenced(ctx context.Context, digest string) (bool, error)
	PutBlob(ctx context.Context, digest string, sizeBytes int64, mediaType *string, storedLocally bool) error

	// Retention + untagged-manifest prune.
	ListOCIPackagesForRetention(ctx context.Context, startAfter string, limit int) ([]database.PackageRetention, error)
	ListTagsForRetention(ctx context.Context, packageID int64) ([]database.TagForRetention, error)
	DeletePackageTag(ctx context.Context, packageID int64, name string) error
	PruneUntaggedManifests(ctx context.Context, olderThan time.Duration, limit int) ([]database.UntaggedManifest, error)
	RecordDeletedManifest(ctx context.Context, digest, repoName, sourceActor string) error
}

type FederationPublisher interface {
	PublishTagDelete(ctx context.Context, repo, tag string) error
	PublishManifestDelete(ctx context.Context, repo, digest string) error
}

// RetentionPolicy applies to OCI packages. Zero KeepLastN/MaxAge disables that knob.
// KeepLastN counts only non-pinned tags — pinned tags are always kept and don't
// consume a slot.
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
	// Per-repo overrides keyed by repo name; non-zero fields win over DB columns.
	RetentionPerRepo      map[string]RetentionPolicy
	RetentionTagsPerCycle int
	// LocalActor is used as source_actor when recording manifest tombstones.
	LocalActor string
	// Namespace is the accountDomain prefix on stored repo names; it lets
	// RetentionPerRepo keys be written relative to the namespace (e.g. "user/app").
	Namespace string

	DiskUsageThreshold     int
	DiskUsageCheckInterval time.Duration
	DiskUsagePath          string
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
	lastRunNS atomic.Int64
	lastDurNS atomic.Int64
	running   atomic.Bool
	diskUsage func(path string) (int, error)
}

// GCStatus is a snapshot of the collector's last-run timing and current state.
// Per-phase counts live in Prometheus metrics.
type GCStatus struct {
	Running         bool       `json:"running"`
	LastRun         *time.Time `json:"lastRun,omitempty"`
	LastDurationMs  int64      `json:"lastDurationMs"`
	IntervalMs      int64      `json:"intervalMs"`
	NextRunEstimate *time.Time `json:"nextRunEstimate,omitempty"`
}

func (gc *GarbageCollector) Status() GCStatus {
	st := GCStatus{
		Running:    gc.running.Load(),
		IntervalMs: gc.cfg.Interval.Milliseconds(),
	}
	if ns := gc.lastRunNS.Load(); ns > 0 {
		last := time.Unix(0, ns)
		st.LastRun = &last
		st.LastDurationMs = time.Duration(gc.lastDurNS.Load()).Milliseconds()
		if gc.cfg.Interval > 0 {
			next := last.Add(gc.cfg.Interval)
			st.NextRunEstimate = &next
		}
	}
	return st
}

func NewGarbageCollector(cfg GCConfig, db GCRepository, blobs blobstore.BlobStore, notifier Notifier, logger *slog.Logger) *GarbageCollector {
	return &GarbageCollector{
		cfg:       cfg,
		db:        db,
		blobs:     blobs,
		notifier:  notifier,
		logger:    logger,
		diskUsage: diskUsedPercent,
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
	tick := gc.cfg.Interval
	if gc.diskTriggerEnabled() && gc.cfg.DiskUsageCheckInterval > 0 && gc.cfg.DiskUsageCheckInterval < tick {
		tick = gc.cfg.DiskUsageCheckInterval
	}

	startup := time.NewTimer(time.Minute)
	defer startup.Stop()

	var ticker *time.Ticker
	var tickerC <-chan time.Time
	for {
		select {
		case <-svcCtx.Done():
			if ticker != nil {
				ticker.Stop()
			}
			return
		case <-parentCtx.Done():
			if ticker != nil {
				ticker.Stop()
			}
			return
		case <-startup.C:
			gc.collect(parentCtx)
			ticker = time.NewTicker(tick)
			tickerC = ticker.C
		case <-tickerC:
			gc.maybeCollect(parentCtx)
		}
	}
}

func (gc *GarbageCollector) diskTriggerEnabled() bool {
	return gc.cfg.DiskUsageThreshold > 0 && gc.cfg.DiskUsagePath != ""
}

func (gc *GarbageCollector) maybeCollect(ctx context.Context) {
	if time.Since(time.Unix(0, gc.lastRunNS.Load())) >= gc.cfg.Interval {
		gc.collect(ctx)
		return
	}
	if !gc.diskTriggerEnabled() {
		return
	}
	pct, err := gc.diskUsage(gc.cfg.DiskUsagePath)
	if err != nil {
		gc.logger.Warn("gc: disk usage check failed", "path", gc.cfg.DiskUsagePath, "error", err)
		return
	}
	metrics.GCDiskUsedPercent.Set(float64(pct))
	if pct < gc.cfg.DiskUsageThreshold {
		return
	}
	gc.logger.Info("gc: disk usage threshold reached, triggering cycle", "used_percent", pct, "threshold", gc.cfg.DiskUsageThreshold)
	metrics.GCDiskTriggered.Add(1)
	gc.collect(ctx)
}

const deletedManifestRetention = 30 * 24 * time.Hour

func (gc *GarbageCollector) collect(ctx context.Context) {
	gc.mu.Lock()
	defer gc.mu.Unlock()

	start := time.Now()
	gc.running.Store(true)
	defer func() {
		end := time.Now()
		gc.lastRunNS.Store(end.UnixNano())
		gc.lastDurNS.Store(end.Sub(start).Nanoseconds())
		gc.running.Store(false)
	}()

	gc.logger.Info("starting garbage collection cycle")

	gc.retentionSweep(ctx)
	gc.pruneUntaggedManifests(ctx)
	gc.cleanupStalePeerBlobs(ctx)
	gc.cleanupOrphanedBlobMetadata(ctx)
	gc.cleanupOrphanedBlobFiles(ctx)
	gc.reconcileBlobStorageDrift(ctx)
	gc.pruneDeletedManifests(ctx)

	metrics.GCCyclesCompleted.Add(1)
	gc.logger.Info("garbage collection cycle complete")
}

// effectiveRetention overlays non-zero fields from the per-repo entry on top
// of the global default.
func (gc *GarbageCollector) effectiveRetention(repo string) RetentionPolicy {
	out := gc.cfg.RetentionDefaults
	cfg, ok := gc.cfg.RetentionPerRepo[repo]
	if !ok && gc.cfg.Namespace != "" {
		// Stored names are namespace-prefixed; allow keys written relative to it.
		if rel, stripped := strings.CutPrefix(repo, gc.cfg.Namespace+"/"); stripped {
			cfg, ok = gc.cfg.RetentionPerRepo[rel]
		}
	}
	if !ok {
		return out
	}
	if cfg.KeepLastN != 0 {
		out.KeepLastN = cfg.KeepLastN
	}
	if cfg.MaxAge != 0 {
		out.MaxAge = cfg.MaxAge
	}
	if cfg.PinnedGlobs != nil {
		out.PinnedGlobs = cfg.PinnedGlobs
	}
	return out
}

// ownsRepo reports whether a package is locally owned. Deletes for peer mirrors
// must not be federated: peers reject them and a tombstone would block re-federation.
func (gc *GarbageCollector) ownsRepo(ownerID string) bool {
	return gc.cfg.LocalActor != "" && ownerID == gc.cfg.LocalActor
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
// federates each delete. Pinned tags are always kept.
func (gc *GarbageCollector) retentionSweep(ctx context.Context) {
	// maxDeletes <= 0 means no cap: paginate every package to exhaustion.
	maxDeletes := gc.cfg.RetentionTagsPerCycle

	totalDeleted := 0
	startAfter := ""
	for maxDeletes <= 0 || totalDeleted < maxDeletes {

		pkgs, err := gc.db.ListOCIPackagesForRetention(ctx, startAfter, retentionPackagePageSize)
		if err != nil {
			gc.logger.Error("gc: retention: list packages failed", "error", err)
			gc.notifier.Send(notify.EventGCError, fmt.Sprintf("GC: retention list packages failed: %v", err))
			return
		}
		if len(pkgs) == 0 {
			break
		}
		for _, pkg := range pkgs {
			if maxDeletes > 0 && totalDeleted >= maxDeletes {
				break
			}
			policy := gc.effectiveRetention(pkg.Name)
			if policy.KeepLastN <= 0 && policy.MaxAge <= 0 {
				continue
			}
			budget := -1 // unlimited
			if maxDeletes > 0 {
				budget = maxDeletes - totalDeleted
			}
			totalDeleted += gc.applyRetentionToPackage(ctx, pkg, policy, budget)
		}
		startAfter = pkgs[len(pkgs)-1].Name
		if len(pkgs) < retentionPackagePageSize {
			break
		}
	}

	if totalDeleted > 0 {
		metrics.GCRetentionTagsDeleted.Add(float64(totalDeleted))
		gc.logger.Info("gc: retention deleted tags", "count", totalDeleted)
	}
}

const retentionPackagePageSize = 100

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
		if tagPinned(t.Name, policy.PinnedGlobs) {
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
		if budget >= 0 && deleted >= budget {
			break
		}
		if err := gc.db.DeletePackageTag(ctx, pkg.ID, t.Name); err != nil {
			gc.logger.Warn("gc: retention: delete tag failed", "package", pkg.Name, "tag", t.Name, "error", err)
			continue
		}
		if gc.publisher != nil && gc.ownsRepo(pkg.OwnerID) {
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

	total := 0
	for {
		rows, err := gc.db.PruneUntaggedManifests(ctx, gc.cfg.UntaggedManifestAge, limit)
		if err != nil {
			gc.logger.Error("gc: prune untagged manifests failed", "error", err)
			gc.notifier.Send(notify.EventGCError, fmt.Sprintf("GC: prune untagged manifests failed: %v", err))
			return
		}
		if len(rows) == 0 {
			break
		}
		total += len(rows)

		for _, r := range rows {
			// Only federate deletes for repos we own; peer mirrors are pruned locally.
			if !gc.ownsRepo(r.OwnerID) {
				continue
			}
			if err := gc.db.RecordDeletedManifest(ctx, r.Digest, r.PackageName, gc.cfg.LocalActor); err != nil {
				gc.logger.Warn("gc: record manifest tombstone failed", "repo", r.PackageName, "digest", r.Digest, "error", err)
			}
			if gc.publisher != nil {
				if err := gc.publisher.PublishManifestDelete(ctx, r.PackageName, r.Digest); err != nil {
					gc.logger.Warn("gc: federate manifest delete failed", "repo", r.PackageName, "digest", r.Digest, "error", err)
				}
			}
		}

		if len(rows) < limit {
			break
		}
	}

	if total > 0 {
		metrics.GCUntaggedManifestsPruned.Add(float64(total))
		gc.logger.Info("gc: pruned untagged manifests", "count", total)
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

// cleanupOrphanedBlobMetadata removes orphan blob rows. The grace period
// excludes rows younger than BlobGCGracePeriod (in-flight uploads).
func (gc *GarbageCollector) cleanupOrphanedBlobMetadata(ctx context.Context) {
	var cutoff time.Time
	if gc.cfg.BlobGCGracePeriod > 0 {
		cutoff = time.Now().Add(-gc.cfg.BlobGCGracePeriod)
	}
	limit := gc.cfg.OrphanBatchSize
	if limit <= 0 {
		limit = 500
	}

	totalRemoved := 0
	for {
		digests, err := gc.db.OrphanedBlobs(ctx, limit, cutoff)
		if err != nil {
			gc.logger.Error("gc: failed to find orphaned blobs", "error", err)
			gc.notifier.Send(notify.EventGCError, fmt.Sprintf("GC: failed to find orphaned blobs: %v", err))
			return
		}
		if len(digests) == 0 {
			break
		}

		removed := 0
		for _, digest := range digests {
			if err := gc.db.DeleteBlob(ctx, digest); err != nil {
				gc.logger.Warn("gc: failed to delete orphaned blob metadata", "digest", digest, "error", err)
				continue
			}
			removed++
		}
		totalRemoved += removed

		// OrphanedBlobs has no cursor; progress relies on deletions shrinking the
		// set, so stop if a batch deleted nothing to avoid spinning.
		if removed == 0 || len(digests) < limit {
			break
		}
	}

	if totalRemoved > 0 {
		metrics.GCOrphanedMetadata.Add(float64(totalRemoved))
		gc.logger.Info("gc: removed orphaned blob metadata", "count", totalRemoved)
	}
}

// Disk-side drift handling. DB-side lives in reconcileBlobStorageDrift.
// Skips files within BlobGCGracePeriod to dodge in-flight-upload races.
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
	repaired := 0
	for _, digest := range diskDigests {
		if knownDigests[digest] {
			continue
		}
		// Repair before delete: a manifest may still reference this digest.
		referenced, err := gc.db.IsBlobReferenced(ctx, digest)
		if err != nil {
			gc.logger.Warn("gc: failed to check blob reference", "digest", digest, "error", err)
			continue
		}
		if referenced {
			size, err := gc.blobs.Size(ctx, digest)
			if err != nil {
				gc.logger.Warn("gc: failed to stat blob for repair", "digest", digest, "error", err)
				continue
			}
			if err := gc.db.PutBlob(ctx, digest, size, nil, true); err != nil {
				gc.logger.Warn("gc: failed to repair blobs row", "digest", digest, "error", err)
				continue
			}
			repaired++
			gc.logger.Info("gc: repaired missing blobs row from disk", "digest", digest, "size", size)
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
	if repaired > 0 {
		metrics.GCBlobRowsRepaired.Add(float64(repaired))
		gc.logger.Info("gc: repaired blobs rows from disk-only state", "count", repaired)
	}
}

const (
	reconcileBlobBatchSize    = 500
	unrecoverableSampleDigest = 5
)

// reconcileBlobStorageDrift is the DB-side leg of drift handling; the disk-side
// leg lives in cleanupOrphanedBlobFiles.
func (gc *GarbageCollector) reconcileBlobStorageDrift(ctx context.Context) {
	var (
		startAfter         string
		degraded, promoted int
		unrecoverable      []string
	)
	for {
		rows, err := gc.db.ListBlobsForReconcile(ctx, startAfter, reconcileBlobBatchSize)
		if err != nil {
			gc.logger.Error("gc: drift reconcile: listing blobs", "error", err)
			gc.notifier.Send(notify.EventGCError, fmt.Sprintf("GC: drift reconcile failed: %v", err))
			return
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			exists, err := gc.blobs.Exists(ctx, row.Digest)
			if err != nil {
				gc.logger.Warn("gc: drift reconcile: stat failed", "digest", row.Digest, "error", err)
				continue
			}
			switch {
			case row.StoredLocally && !exists:
				// Re-stat before degrading: the first miss may be transient (a
				// delete-then-reupload, an interrupted Put about to retry, or an
				// S3 read-after-write window). Degrading on a transient miss would
				// route reads to a peer that may not hold the blob.
				if recheck, rerr := gc.blobs.Exists(ctx, row.Digest); rerr == nil && recheck {
					continue
				}
				hasPeer, err := gc.db.HasPeerBlob(ctx, row.Digest)
				if err != nil {
					gc.logger.Warn("gc: drift reconcile: peer lookup failed", "digest", row.Digest, "error", err)
					continue
				}
				if hasPeer {
					if err := gc.db.SetBlobStoredLocally(ctx, row.Digest, false); err != nil {
						gc.logger.Warn("gc: drift reconcile: degrade failed", "digest", row.Digest, "error", err)
						continue
					}
					degraded++
					gc.logger.Warn("gc: drift reconcile: file missing, degraded to peer redirect", "digest", row.Digest)
					continue
				}
				unrecoverable = append(unrecoverable, row.Digest)
				gc.logger.Error("gc: drift reconcile: file missing, no peer holds it", "digest", row.Digest)
			case !row.StoredLocally && exists:
				if err := gc.db.SetBlobStoredLocally(ctx, row.Digest, true); err != nil {
					gc.logger.Warn("gc: drift reconcile: promote failed", "digest", row.Digest, "error", err)
					continue
				}
				promoted++
				gc.logger.Info("gc: drift reconcile: file present, promoted to stored_locally", "digest", row.Digest)
			}
		}
		if len(rows) < reconcileBlobBatchSize {
			break
		}
		startAfter = rows[len(rows)-1].Digest
	}
	if degraded > 0 {
		metrics.GCBlobDriftDegraded.Add(float64(degraded))
	}
	if promoted > 0 {
		metrics.GCBlobDriftPromoted.Add(float64(promoted))
	}
	if n := len(unrecoverable); n > 0 {
		metrics.GCBlobDriftUnrecoverable.Add(float64(n))
		// Coalesced to one notification per cycle: a disk-wipe scenario could
		// otherwise fan out thousands of per-digest pages.
		sample := unrecoverable
		if n > unrecoverableSampleDigest {
			sample = sample[:unrecoverableSampleDigest]
		}
		gc.notifier.Send(notify.EventGCError,
			fmt.Sprintf("GC: %d blob(s) have file missing and no peer holds them; sample: %s", n, strings.Join(sample, ", ")))
	}
}

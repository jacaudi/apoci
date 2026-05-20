package metrics

import "github.com/prometheus/client_golang/prometheus"

const (
	namespace         = "apoci"
	subsystemInbox    = "inbox"
	subsystemDelivery = "delivery"
	subsystemBlobRepl = "blob_replication"
	subsystemRegistry = "registry"
	subsystemUpstream = "upstream"
)

var (
	// Inbox: inbound activity counters by type.
	InboxActivities = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: subsystemInbox,
		Name:      "activities_total",
		Help:      "Total inbound activities by type.",
	}, []string{"type"})
	InboxDedupHits = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: subsystemInbox,
		Name:      "dedup_hits_total",
		Help:      "Total duplicate activities dropped.",
	})
	InboxRateLimited = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: subsystemInbox,
		Name:      "rate_limited_total",
		Help:      "Total inbound requests rejected by rate limiter.",
	})

	// Publisher: outbound activity counters.
	OutboundActivities = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "outbound",
		Name:      "activities_total",
		Help:      "Total outbound activities by type.",
	}, []string{"type"})

	// Delivery queue.
	DeliveryEnqueued = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: subsystemDelivery,
		Name:      "enqueued_total",
		Help:      "Total deliveries enqueued.",
	})
	DeliverySucceeded = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: subsystemDelivery,
		Name:      "succeeded_total",
		Help:      "Total deliveries succeeded, labelled by peer domain.",
	}, []string{"domain"})
	DeliveryFailed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: subsystemDelivery,
		Name:      "failed_total",
		Help:      "Total deliveries permanently failed, labelled by peer domain.",
	}, []string{"domain"})
	DeliveryRetries = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: subsystemDelivery,
		Name:      "retries_total",
		Help:      "Total delivery retries.",
	})
	DeliveryPending = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: subsystemDelivery,
		Name:      "pending",
		Help:      "Number of deliveries currently pending.",
	})
	DeliveryCircuitOpen = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: subsystemDelivery,
		Name:      "circuit_open_domains",
		Help:      "Number of peer domains currently circuit-broken (delivery skipped).",
	})

	// Blob replication.
	BlobReplicationsStarted = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: subsystemBlobRepl,
		Name:      "started_total",
		Help:      "Total blob replications started.",
	})
	BlobReplicationsSucceeded = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: subsystemBlobRepl,
		Name:      "succeeded_total",
		Help:      "Total blob replications succeeded.",
	})
	BlobReplicationsFailed = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: subsystemBlobRepl,
		Name:      "failed_total",
		Help:      "Total blob replications failed.",
	})
	BlobReplicationsInFlight = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: subsystemBlobRepl,
		Name:      "in_flight",
		Help:      "Number of blob replications currently in progress.",
	})

	// Garbage collection.
	GCStalePeerBlobs = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "gc",
		Name:      "stale_peer_blobs_total",
		Help:      "Total stale peer blobs removed.",
	})
	GCOrphanedMetadata = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "gc",
		Name:      "orphaned_metadata_total",
		Help:      "Total orphaned metadata entries removed.",
	})
	GCOrphanedFiles = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "gc",
		Name:      "orphaned_files_total",
		Help:      "Total orphaned files removed.",
	})
	GCCyclesCompleted = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "gc",
		Name:      "cycles_completed_total",
		Help:      "Total GC cycles completed.",
	})
	GCRetentionTagsDeleted = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "gc",
		Name:      "retention_tags_deleted_total",
		Help:      "Total tags deleted by retention policy.",
	})
	GCUntaggedManifestsPruned = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "gc",
		Name:      "untagged_manifests_pruned_total",
		Help:      "Total untagged manifests pruned.",
	})
	GCDiskUsedPercent = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: "gc",
		Name:      "disk_used_percent",
		Help:      "Last observed filesystem usage percentage of the blob volume.",
	})
	GCDiskTriggered = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "gc",
		Name:      "disk_triggered_total",
		Help:      "Total GC cycles triggered by disk usage threshold.",
	})
	GCBlobDriftDegraded = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "gc",
		Name:      "blob_drift_degraded_total",
		Help:      "Blobs flipped to stored_locally=false because the file vanished and a peer has it.",
	})
	GCBlobDriftPromoted = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "gc",
		Name:      "blob_drift_promoted_total",
		Help:      "Blobs flipped to stored_locally=true after the file was found on disk.",
	})
	GCBlobDriftUnrecoverable = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "gc",
		Name:      "blob_drift_unrecoverable_total",
		Help:      "Blobs whose file is missing and no peer is known to hold a copy.",
	})
	GCBlobRowsRepaired = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "gc",
		Name:      "blob_rows_repaired_total",
		Help:      "Blob rows reinserted from disk because a manifest still referenced the digest.",
	})

	// OCI registry operations.
	RegistryManifestPushes = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: subsystemRegistry,
		Name:      "manifest_pushes_total",
		Help:      "Total manifest pushes.",
	})
	RegistryManifestPulls = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: subsystemRegistry,
		Name:      "manifest_pulls_total",
		Help:      "Total manifest pulls.",
	})
	RegistryBlobPushes = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: subsystemRegistry,
		Name:      "blob_pushes_total",
		Help:      "Total blob pushes.",
	})
	RegistryBlobPulls = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: subsystemRegistry,
		Name:      "blob_pulls_total",
		Help:      "Total blob pulls.",
	})
	RegistryBlobPullThru = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: subsystemRegistry,
		Name:      "blob_pull_throughs_total",
		Help:      "Total blob pull-throughs from peers.",
	})
	RegistryBlobMounts = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: subsystemRegistry,
		Name:      "blob_mounts_total",
		Help:      "Total cross-repository blob mounts.",
	})
	RegistryManifestPullThru = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: subsystemRegistry,
		Name:      "manifest_pull_throughs_total",
		Help:      "Total manifest pull-throughs from peers.",
	})
	RegistryPushRateLimited = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: subsystemRegistry,
		Name:      "push_rate_limited_total",
		Help:      "Total pushes rejected by rate limiter.",
	})

	// Upstream proxy metrics.
	UpstreamBlobPullThru = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: subsystemUpstream,
		Name:      "blob_pull_throughs_total",
		Help:      "Total blob pull-throughs from upstream registries.",
	}, []string{subsystemRegistry})
	UpstreamManifestPullThru = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: subsystemUpstream,
		Name:      "manifest_pull_throughs_total",
		Help:      "Total manifest pull-throughs from upstream registries.",
	}, []string{subsystemRegistry})
	UpstreamCircuitOpen = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: subsystemUpstream,
		Name:      "circuit_open",
		Help:      "Whether circuit breaker is open for an upstream registry (1=open, 0=closed).",
	}, []string{subsystemRegistry})

	// Latency histograms.
	latencyBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

	DeliveryDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace,
		Subsystem: subsystemDelivery,
		Name:      "duration_seconds",
		Help:      "Duration of individual activity delivery attempts.",
		Buckets:   latencyBuckets,
	})
	BlobFetchDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace,
		Subsystem: subsystemBlobRepl,
		Name:      "fetch_duration_seconds",
		Help:      "Duration of blob fetch operations from peers.",
		Buckets:   latencyBuckets,
	})
	InboxProcessingDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace,
		Subsystem: subsystemInbox,
		Name:      "processing_duration_seconds",
		Help:      "Duration of inbox activity processing.",
		Buckets:   latencyBuckets,
	})
	PeerFetchDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace,
		Subsystem: subsystemRegistry,
		Name:      "peer_fetch_duration_seconds",
		Help:      "Duration of pull-through manifest/blob fetches from peers.",
		Buckets:   latencyBuckets,
	})

	// Federation state (gauges).
	FederationFollowers = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: "federation",
		Name:      "followers",
		Help:      "Current number of followers.",
	})
	FederationFollowing = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: "federation",
		Name:      "following",
		Help:      "Current number of peers being followed.",
	})
)

func init() {
	prometheus.MustRegister(
		InboxActivities,
		InboxDedupHits,
		InboxRateLimited,
		OutboundActivities,
		DeliveryEnqueued,
		DeliverySucceeded,
		DeliveryFailed,
		DeliveryRetries,
		DeliveryPending,
		DeliveryCircuitOpen,
		BlobReplicationsStarted,
		BlobReplicationsSucceeded,
		BlobReplicationsFailed,
		BlobReplicationsInFlight,
		GCStalePeerBlobs,
		GCOrphanedMetadata,
		GCOrphanedFiles,
		GCCyclesCompleted,
		GCRetentionTagsDeleted,
		GCUntaggedManifestsPruned,
		GCDiskUsedPercent,
		GCDiskTriggered,
		GCBlobDriftDegraded,
		GCBlobDriftPromoted,
		GCBlobDriftUnrecoverable,
		GCBlobRowsRepaired,
		RegistryManifestPushes,
		RegistryManifestPulls,
		RegistryBlobPushes,
		RegistryBlobPulls,
		RegistryBlobPullThru,
		RegistryBlobMounts,
		RegistryManifestPullThru,
		RegistryPushRateLimited,
		UpstreamBlobPullThru,
		UpstreamManifestPullThru,
		UpstreamCircuitOpen,
		DeliveryDuration,
		BlobFetchDuration,
		InboxProcessingDuration,
		PeerFetchDuration,
		FederationFollowers,
		FederationFollowing,
	)
}

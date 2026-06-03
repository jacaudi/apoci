package server

import (
	"context"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/time/rate"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/activitypub"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/blobstore"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/config"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/federation"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/metrics"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/notify"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/oci"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/peering"
	pkgreg "git.erwanleboucher.dev/eleboucher/apoci/internal/registry"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/registry/cargo"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/registry/npm"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/registry/pypi"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/replication"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/scanner"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/upstream"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/validate"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/workers"
)

type Server struct {
	cfg                 *config.Config
	db                  *database.DB
	blobs               blobstore.BlobStore
	identity            *activitypub.Identity
	publisher           *activitypub.APPublisher
	fedSvc              *federation.Service
	registry            *oci.Registry
	workers             *workers.Workers
	deliveryQueue       *activitypub.DeliveryQueue
	gc                  *peering.GarbageCollector
	replication         *replication.Worker
	ociHandler          http.Handler
	actorHandler        http.Handler
	webfingerHandler    http.Handler
	nodeinfoHandler     *activitypub.NodeInfoHandler
	inboxHandler        *activitypub.InboxHandler
	outboxHandler       http.Handler
	followersHandler    http.Handler
	followingHandler    http.Handler
	inboxLimiter        *ipRateLimiter
	registryPushLimiter *ipRateLimiter
	httpServer          *http.Server
	packageBackends     *pkgreg.Manager
	uiTemplates         *template.Template
	logger              *slog.Logger
}

// peerHealthAdapter bridges *database.DB to peering.HealthRepository by
// converting between database.Actor and peering.PeerRecord.
type peerHealthAdapter struct {
	db *database.DB
}

func toRetentionMap(in []config.RepoRetention) map[string]peering.RetentionPolicy {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]peering.RetentionPolicy, len(in))
	for _, r := range in {
		out[r.Repo] = peering.RetentionPolicy{
			KeepLastN:   r.KeepLastN,
			MaxAge:      r.MaxAge,
			PinnedGlobs: r.PinnedGlobs,
		}
	}
	return out
}

func (a *peerHealthAdapter) ListAllPeers(ctx context.Context) ([]peering.PeerRecord, error) {
	peers, err := a.db.ListAllPeers(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing peers: %w", err)
	}
	records := make([]peering.PeerRecord, len(peers))
	for i, p := range peers {
		records[i] = peering.PeerRecord{
			ActorURL:  p.ActorURL,
			Endpoint:  p.Endpoint,
			IsHealthy: p.IsHealthy,
		}
	}
	return records, nil
}

func (a *peerHealthAdapter) SetPeerHealth(ctx context.Context, actorURL string, healthy bool) error {
	return a.db.SetPeerHealth(ctx, actorURL, healthy)
}

func New(cfg *config.Config, db *database.DB, blobs blobstore.BlobStore, identity *activitypub.Identity, version string, logger *slog.Logger) (*Server, error) {
	if cfg.Federation.AllowInsecureHTTP {
		activitypub.SetAllowInsecureHTTP(true)
		validate.AllowPrivateIPs.Store(true)
		logger.Warn("insecure HTTP federation enabled")
	}

	registry, err := oci.NewRegistry(db, blobs, identity.ActorURL, cfg.AccountDomain, cfg.Limits.MaxManifestSize, cfg.Limits.MaxBlobSize, logger)
	if err != nil {
		return nil, err
	}

	// Stage in-progress chunked uploads on the data volume (local backend) or
	// the configured S3 temp dir, falling back to the OS temp dir. Either way
	// uploads stream to disk rather than the heap; operators on S3 should set
	// storage.s3.tempDir to a large volume so big pushes don't fill the
	// container's ephemeral storage.
	uploadDir := os.TempDir()
	switch {
	case cfg.BlobDiskPath() != "":
		uploadDir = filepath.Join(cfg.DataDir, "uploads")
	case cfg.Storage.S3.TempDir != "":
		uploadDir = cfg.Storage.S3.TempDir
	}
	if err := registry.SetUploadDir(uploadDir); err != nil {
		return nil, err
	}

	notifier := notify.New(cfg.Name, cfg.Notifications.URLs, cfg.Notifications.Events, logger)

	apPublisher := activitypub.NewAPPublisher(identity, db, cfg.Endpoint, cfg.Federation.ExcludedRepos, logger)
	apResolver := activitypub.NewAPResolver(db, logger)
	deliveryQueue := activitypub.NewDeliveryQueue(db, identity, logger)
	fetcher := peering.NewFetcher(cfg.Peering.FetchTimeout, cfg.Limits.MaxBlobSize, cfg.Limits.MaxManifestSize, logger)
	healthChecker := peering.NewHealthChecker(&peerHealthAdapter{db: db}, fetcher, cfg.Peering.HealthCheckInterval, notifier, logger)

	blobReplicator := peering.NewBlobReplicator(db, blobs, fetcher, notifier, logger)
	diskPath := cfg.BlobDiskPath()
	if cfg.GC.DiskUsageThreshold > 0 && diskPath == "" {
		logger.Warn("gc.diskUsageThreshold ignored: storage backend has no local filesystem", "storage_type", cfg.Storage.Type)
	}
	gc := peering.NewGarbageCollector(peering.GCConfig{
		Interval:               cfg.GC.Interval,
		StalePeerBlobAge:       cfg.GC.StalePeerBlobAge,
		OrphanBatchSize:        cfg.GC.OrphanBatchSize,
		BlobGCGracePeriod:      cfg.GC.BlobGCGracePeriod,
		UntaggedManifestAge:    cfg.GC.UntaggedManifestAge,
		UntaggedBatchSize:      cfg.GC.UntaggedBatchSize,
		RetentionTagsPerCycle:  cfg.GC.RetentionTagsPerCycle,
		DiskUsageThreshold:     cfg.GC.DiskUsageThreshold,
		DiskUsageCheckInterval: cfg.GC.DiskUsageCheckInterval,
		DiskUsagePath:          diskPath,
		RetentionDefaults: peering.RetentionPolicy{
			KeepLastN:   cfg.GC.Retention.KeepLastN,
			MaxAge:      cfg.GC.Retention.MaxAge,
			PinnedGlobs: cfg.GC.Retention.PinnedGlobs,
		},
		RetentionPerRepo: toRetentionMap(cfg.GC.Retention.PerRepo),
		LocalActor:       identity.ActorURL,
	}, db, blobs, notifier, logger)
	gc.SetFederationPublisher(apPublisher)

	apPublisher.SetNotifyFunc(deliveryQueue.Notify)

	registry.SetPublisher(apPublisher)
	registry.SetFederation(apResolver, fetcher)

	// Initialize upstream proxy if enabled
	if cfg.Upstreams.Enabled && len(cfg.Upstreams.Registries) > 0 {
		upstreamFetcher := upstream.NewFetcher(cfg.Upstreams, cfg.Limits.MaxBlobSize, cfg.Limits.MaxManifestSize, logger)
		registry.SetUpstreamFetcher(upstreamFetcher)
		logger.Info("upstream proxy enabled", "registries", len(cfg.Upstreams.Registries))
	}

	enqueueFunc := activitypub.EnqueueFunc(func(ctx context.Context, activityID, inboxURL string, activityJSON []byte) error {
		if err := db.EnqueueDelivery(ctx, activityID, inboxURL, activityJSON); err != nil {
			return err
		}
		deliveryQueue.Notify()
		return nil
	})

	inboxHandler := activitypub.NewInboxHandler(identity, db, activitypub.InboxConfig{
		MaxManifestSize: cfg.Limits.MaxManifestSize,
		MaxBlobSize:     cfg.Limits.MaxBlobSize,
		AutoAccept:      cfg.Federation.AutoAccept,
		AllowedDomains:  cfg.Federation.AllowedDomains,
		BlockedDomains:  cfg.Federation.BlockedDomains,
		BlockedActors:   cfg.Federation.BlockedActors,
	}, logger)
	inboxHandler.SetBlobReplicator(blobReplicator)
	inboxHandler.SetEnqueueFunc(enqueueFunc)
	inboxHandler.SetActorCache(apPublisher.ActorCache())
	inboxHandler.SetNotifier(notifier)

	packageBackends := pkgreg.NewManager()
	adapters := activitypub.NewAdapterRegistry()

	type backendInit struct {
		name  string
		cfg   config.BackendConfig
		build func(pub activitypub.PackagePublisher) (pkgreg.Backend, activitypub.FederationAdapter)
	}

	inits := []backendInit{
		{"npm", cfg.Backends.NPM, func(pub activitypub.PackagePublisher) (pkgreg.Backend, activitypub.FederationAdapter) {
			b := npm.New(npm.Config{
				DB: db, Blobs: blobs, Endpoint: cfg.Endpoint, Owner: identity.ActorURL,
				Token: cfg.Backends.NPM.TokenOr(cfg.RegistryToken), Publisher: pub,
				Replicator: blobReplicator, Logger: logger,
			})
			return b, b.FederationAdapter()
		}},
		{"cargo", cfg.Backends.Cargo, func(pub activitypub.PackagePublisher) (pkgreg.Backend, activitypub.FederationAdapter) {
			b := cargo.New(cargo.Config{
				DB: db, Blobs: blobs, Endpoint: cfg.Endpoint, Owner: identity.ActorURL,
				Token: cfg.Backends.Cargo.TokenOr(cfg.RegistryToken), Publisher: pub,
				Replicator: blobReplicator, Logger: logger,
			})
			return b, b.FederationAdapter()
		}},
		{"pypi", cfg.Backends.PyPI, func(pub activitypub.PackagePublisher) (pkgreg.Backend, activitypub.FederationAdapter) {
			b := pypi.New(pypi.Config{
				DB: db, Blobs: blobs, Endpoint: cfg.Endpoint, Owner: identity.ActorURL,
				Token: cfg.Backends.PyPI.TokenOr(cfg.RegistryToken), Publisher: pub,
				Replicator: blobReplicator, Logger: logger,
			})
			return b, b.FederationAdapter()
		}},
	}

	for _, init := range inits {
		if !init.cfg.IsEnabled() {
			continue
		}
		var pub activitypub.PackagePublisher
		if init.cfg.IsFederated() {
			pub = apPublisher
		}
		b, adapter := init.build(pub)
		if err := packageBackends.Register(b); err != nil {
			return nil, fmt.Errorf("registering %s backend: %w", init.name, err)
		}
		if init.cfg.IsFederated() {
			if err := adapters.Register(adapter); err != nil {
				return nil, fmt.Errorf("registering %s adapter: %w", init.name, err)
			}
		}
	}

	inboxHandler.SetAdapters(adapters)

	inboxWorker := activitypub.NewInboxWorker(inboxHandler, logger)
	inboxHandler.SetWorker(inboxWorker)

	inboxLimiter := newIPRateLimiter(
		rate.Limit(cfg.RateLimits.InboxRate),
		cfg.RateLimits.InboxBurst,
		cfg.RateLimits.TrustedIPs,
	)
	registryPushLimiter := newIPRateLimiter(
		rate.Limit(cfg.RateLimits.RegistryPushRate),
		cfg.RateLimits.RegistryPushBurst,
		cfg.RateLimits.TrustedIPs,
	)

	scheduler := workers.NewScheduler(logger)
	scheduler.Add(workers.PeriodicTask{
		Interval: 5 * time.Minute,
		Fn: func(ctx context.Context) {
			if _, err := registry.CleanExpiredUploads(ctx); err != nil {
				logger.Warn("upload session cleanup failed", "error", err)
			}
		},
	})
	scheduler.Add(workers.PeriodicTask{
		Interval: 1 * time.Hour,
		Fn: func(ctx context.Context) {
			n, err := db.DeleteStaleOutgoingFollows(ctx,
				cfg.Federation.OutgoingFollowPendingTTL,
				cfg.Federation.OutgoingFollowRejectedTTL)
			if err != nil {
				logger.Warn("stale outgoing follow cleanup failed", "error", err)
			} else if n > 0 {
				logger.Info("cleaned up stale outgoing follows", "count", n)
			}
		},
	})
	services := []workers.Service{healthChecker, scheduler}
	if *cfg.GC.Enabled {
		services = append(services, gc)
	}

	if cfg.Scanner.Enabled {
		scanWorker := scanner.NewWorker(scanner.Config{
			Scanner: scanner.NewTrivy(scanner.TrivyConfig{
				BinaryPath: cfg.Scanner.Trivy.BinaryPath,
				Insecure:   cfg.Scanner.Trivy.Insecure,
				Username:   "apoci",
				Password:   cfg.RegistryToken,
			}, logger),
			Registry:  registry,
			Host:      stripScheme(cfg.Endpoint),
			Timeout:   cfg.Scanner.Timeout,
			QueueSize: cfg.Scanner.QueueSize,
		}, logger)
		registry.AddManifestObserver(scanWorker)
		services = append(services, scanWorker)
		logger.Info("inline vulnerability scanning enabled", "scanner", "trivy")
	}

	var replWorker *replication.Worker
	if cfg.Replication.Enabled && len(cfg.Replication.Targets) > 0 {
		targets := make([]replication.Target, len(cfg.Replication.Targets))
		for i, t := range cfg.Replication.Targets {
			targets[i] = replication.Target{
				Name:          t.Name,
				Endpoint:      t.Endpoint,
				Auth:          t.Auth,
				Username:      t.Username,
				Password:      t.Password,
				Insecure:      t.Insecure,
				RepoGlobs:     t.RepoGlobs,
				StripPrefix:   t.StripPrefix,
				DestNamespace: t.DestNamespace,
			}
		}
		replWorker = replication.NewWorker(replication.Config{
			Targets:   targets,
			Source:    registry,
			Timeout:   cfg.Replication.Timeout,
			QueueSize: 1000,
		}, logger)
		registry.AddManifestObserver(replWorker)
		services = append(services, replWorker)
		logger.Info("outbound replication enabled", "targets", len(targets))
	}

	w := &workers.Workers{
		Services:   services,
		Waiters:    []workers.Waiter{blobReplicator},
		Drainables: []workers.Service{inboxWorker, deliveryQueue},
		Cleanup:    []workers.Stoppable{notifier, inboxHandler, inboxLimiter, registryPushLimiter, apPublisher},
		Logger:     logger,
	}

	s := &Server{
		cfg:           cfg,
		db:            db,
		blobs:         blobs,
		identity:      identity,
		publisher:     apPublisher,
		deliveryQueue: deliveryQueue,
		fedSvc: &federation.Service{
			Fed:      &federation.RealFederator{Identity: identity, Enqueue: enqueueFunc},
			DB:       db,
			ActorURL: identity.ActorURL,
			Logger:   logger,
		},
		registry:            registry,
		gc:                  gc,
		replication:         replWorker,
		packageBackends:     packageBackends,
		workers:             w,
		ociHandler:          registry.Handler(),
		actorHandler:        activitypub.NewActorHandler(identity, cfg.Name, cfg.Endpoint),
		webfingerHandler:    activitypub.NewWebFingerHandler(identity),
		nodeinfoHandler:     activitypub.NewNodeInfoHandler(identity.Domain, version),
		inboxHandler:        inboxHandler,
		outboxHandler:       activitypub.NewOutboxHandler(identity, db),
		followersHandler:    activitypub.NewFollowersHandler(identity, db),
		followingHandler:    activitypub.NewFollowingHandler(identity, db),
		inboxLimiter:        inboxLimiter,
		registryPushLimiter: registryPushLimiter,
		logger:              logger,
	}

	if cfg.UI.Enabled {
		if err := s.initUITemplates(); err != nil {
			return nil, fmt.Errorf("initializing UI templates: %w", err)
		}
	}

	scheduler.Add(workers.PeriodicTask{
		Interval: 24 * time.Hour,
		Fn:       s.fedSvc.RefreshActors,
	})

	s.httpServer = &http.Server{
		Handler:           s.routes(),
		ReadTimeout:       30 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	return s, nil
}

func (s *Server) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", s.cfg.Listen, err)
	}

	s.workers.Start(ctx)

	go s.deliveryQueue.PreWarmCircuit(ctx)
	go s.fedSvc.RefreshActors(ctx)         //nolint:gosec // initial refresh on startup before first periodic run
	go s.runFederationWithdrawalSweep(ctx) //nolint:gosec

	if s.cfg.Metrics.Enabled {
		metricsMux := http.NewServeMux()
		metricsMux.Handle("/metrics", promhttp.Handler())
		var metricsHandler http.Handler = metricsMux
		if s.cfg.Metrics.Token != "" {
			metricsHandler = bearerAuthMiddleware(s.cfg.Metrics.Token)(metricsMux)
		}
		metricsServer := &http.Server{
			Addr:              s.cfg.Metrics.Listen,
			Handler:           metricsHandler,
			ReadTimeout:       5 * time.Second,
			ReadHeaderTimeout: 5 * time.Second,
			WriteTimeout:      10 * time.Second,
			IdleTimeout:       30 * time.Second,
		}
		go func() { //nolint:gosec // intentional background goroutine for metrics server
			s.logger.Info("metrics server listening", "address", s.cfg.Metrics.Listen)
			if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				s.logger.Error("metrics server error", "error", err)
			}
		}()
		go func() { //nolint:gosec // intentional background goroutine for graceful shutdown
			<-ctx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = metricsServer.Shutdown(shutdownCtx)
		}()
	}

	follows, err := s.db.ListFollows(ctx)
	if err != nil {
		s.logger.Warn("failed to count followers", "error", err)
	}
	outgoing, err := s.db.ListOutgoingFollows(ctx, "accepted")
	if err != nil {
		s.logger.Warn("failed to count following", "error", err)
	}
	metrics.FederationFollowers.Set(float64(len(follows)))
	metrics.FederationFollowing.Set(float64(len(outgoing)))

	s.logger.Info("OCI registry listening",
		"address", ln.Addr().String(),
		"node", s.cfg.Name,
		"actor", s.identity.ActorURL,
		"followers", len(follows),
		"following", len(outgoing),
	)

	shutdownDone := make(chan struct{})
	go func() { //nolint:gosec // intentional background goroutine for graceful shutdown
		defer close(shutdownDone)
		<-ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		s.logger.Info("shutting down HTTP server")
		_ = s.httpServer.Shutdown(shutdownCtx)

		s.workers.Stop()
	}()

	var serveErr error
	if s.cfg.TLS != nil {
		serveErr = s.httpServer.ServeTLS(ln, s.cfg.TLS.Cert, s.cfg.TLS.Key)
	} else {
		serveErr = s.httpServer.Serve(ln)
	}
	<-shutdownDone
	return serveErr
}

// Stamps federation_withdrawn_at so subsequent restarts don't re-emit.
func (s *Server) runFederationWithdrawalSweep(ctx context.Context) {
	globs := s.cfg.Federation.ExcludedRepos
	if len(globs) == 0 {
		return
	}
	pending, err := s.db.ListPackagesPendingWithdrawal(ctx, "oci")
	if err != nil {
		s.logger.Error("withdrawal sweep: listing pending packages", "error", err)
		return
	}
	for _, pkg := range pending {
		matched := false
		for _, g := range globs {
			if ok, err := path.Match(g, pkg.Name); err == nil && ok {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		tags, err := s.db.ListPackageTags(ctx, pkg.ID)
		if err != nil {
			s.logger.Error("withdrawal sweep: listing tags", "repo", pkg.Name, "error", err)
			continue
		}
		versions, err := s.db.ListPackageVersions(ctx, pkg.ID)
		if err != nil {
			s.logger.Error("withdrawal sweep: listing versions", "repo", pkg.Name, "error", err)
			continue
		}
		tagNames := make([]string, len(tags))
		for i, t := range tags {
			tagNames[i] = t.Name
		}
		digests := make([]string, len(versions))
		for i, v := range versions {
			digests[i] = v.Version
		}
		if err := s.publisher.WithdrawRepo(ctx, pkg.Name, tagNames, digests); err != nil {
			s.logger.Error("withdrawal sweep: emitting deletes", "repo", pkg.Name, "error", err)
			continue
		}
		if err := s.db.MarkPackageWithdrawn(ctx, pkg.ID); err != nil {
			s.logger.Error("withdrawal sweep: marking withdrawn", "repo", pkg.Name, "error", err)
			continue
		}
		s.logger.Info("withdrawal sweep: retracted repo from peers", "repo", pkg.Name, "tags", len(tagNames), "manifests", len(digests))
	}
}

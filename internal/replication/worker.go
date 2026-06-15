package replication

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"path"
	"sync"
	"time"

	"codeberg.org/gruf/go-runners"
	"cuelabs.dev/go/oci/ociregistry"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/queue"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/util"
)

// Source reads local manifests and blobs to replicate. *oci.Registry satisfies it.
type Source interface {
	GetManifest(ctx context.Context, repo string, digest ociregistry.Digest) (ociregistry.BlobReader, error)
	GetBlob(ctx context.Context, repo string, digest ociregistry.Digest) (ociregistry.BlobReader, error)
}

type job struct {
	targetIdx int
	repo      string
	tag       string
	digest    string
	mediaType string
}

// TargetStatus is a snapshot of one target's replication activity.
type TargetStatus struct {
	Name       string     `json:"name"`
	LastRun    *time.Time `json:"lastRun,omitempty"`
	LastError  string     `json:"lastError,omitempty"`
	Replicated int64      `json:"replicated"`
	Failed     int64      `json:"failed"`
	Dropped    int64      `json:"dropped"`
}

type targetState struct {
	mu         sync.Mutex
	lastRunNS  int64
	lastError  string
	replicated int64
	failed     int64
	dropped    int64
}

// Config configures a replication Worker.
type Config struct {
	Targets   []Target
	Source    Source
	Timeout   time.Duration
	QueueSize int
}

// Worker replicates pushed manifests to external OCI targets. It implements
// oci.ManifestObserver and workers.Service.
type Worker struct {
	targets  []Target
	clients  []*Client
	states   []*targetState
	source   Source
	timeout  time.Duration
	maxQueue int
	queues   []*queue.SimpleQueue[job]
	logger   *slog.Logger
	service  runners.Service
}

// defaultProcessTimeout bounds a single manifest replication when no
// Replication.Timeout is configured, so a hung target cannot stall shutdown
// or a worker indefinitely.
const defaultProcessTimeout = 5 * time.Minute

func NewWorker(cfg Config, logger *slog.Logger) *Worker {
	clients := make([]*Client, len(cfg.Targets))
	states := make([]*targetState, len(cfg.Targets))
	queues := make([]*queue.SimpleQueue[job], len(cfg.Targets))
	for i, t := range cfg.Targets {
		clients[i] = NewClient(t, cfg.Timeout)
		states[i] = &targetState{}
		queues[i] = queue.NewSimpleQueue[job](cfg.QueueSize)
	}
	return &Worker{
		targets:  cfg.Targets,
		clients:  clients,
		states:   states,
		source:   cfg.Source,
		timeout:  cfg.Timeout,
		maxQueue: cfg.QueueSize,
		queues:   queues,
		logger:   logger.With("component", "replication"),
	}
}

// OnManifestPushed enqueues a replication job onto each matching target's own
// queue. Per-target queues keep a slow or unreachable target from filling a
// shared queue and starving healthy targets.
func (w *Worker) OnManifestPushed(repo, tag, digest, mediaType string, _ *string) {
	for i, t := range w.targets {
		if !matchRepo(t, repo) {
			continue
		}
		if !w.queues[i].TryPush(job{targetIdx: i, repo: repo, tag: tag, digest: digest, mediaType: mediaType}, w.maxQueue) {
			w.logger.Warn("replication queue full, dropping job", "target", t.Name, "repo", repo, "digest", digest)
			st := w.states[i]
			st.mu.Lock()
			st.dropped++
			st.mu.Unlock()
		}
	}
}

func matchRepo(t Target, repo string) bool {
	if len(t.RepoGlobs) == 0 {
		return true
	}
	for _, g := range t.RepoGlobs {
		if ok, err := path.Match(g, repo); err == nil && ok {
			return true
		}
	}
	return false
}

func (w *Worker) Start(_ context.Context) {
	w.service.GoRun(func(svcCtx context.Context) {
		var wg sync.WaitGroup
		for i := range w.targets {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				util.Must(w.logger, func() { w.run(svcCtx, idx) })
			}(i)
		}
		wg.Wait()
	})
}

func (w *Worker) Stop() {
	w.service.Stop()
}

func (w *Worker) run(svcCtx context.Context, idx int) {
	q := w.queues[idx]
	for {
		j, ok := q.PopCtx(svcCtx)
		if !ok {
			return
		}
		w.process(svcCtx, j)
	}
}

func (w *Worker) process(ctx context.Context, j job) {
	target := w.targets[j.targetIdx]
	state := w.states[j.targetIdx]

	timeout := w.timeout
	if timeout <= 0 {
		timeout = defaultProcessTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	err := w.replicate(ctx, j.targetIdx, j.repo, j.tag, j.digest, j.mediaType, map[string]bool{})

	state.mu.Lock()
	state.lastRunNS = time.Now().UnixNano()
	if err != nil {
		state.lastError = err.Error()
		state.failed++
	} else {
		state.lastError = ""
		state.replicated++
	}
	state.mu.Unlock()

	if err != nil {
		w.logger.Error("replication failed", "target", target.Name, "repo", j.repo, "digest", j.digest, "error", err)
		return
	}
	w.logger.Info("replicated manifest", "target", target.Name, "repo", j.repo, "tag", j.tag, "digest", j.digest)
}

// replicate pushes a manifest and everything it references to one target. It
// recurses into child manifests (indexes) and dedupes via visited.
func (w *Worker) replicate(ctx context.Context, idx int, repo, tag, digest, mediaType string, visited map[string]bool) error {
	if visited[digest] {
		return nil
	}
	visited[digest] = true

	target := w.targets[idx]
	client := w.clients[idx]
	destRepo := target.DestRepo(repo)

	body, err := w.readManifest(ctx, repo, digest)
	if err != nil {
		return err
	}
	blobs, children := parseRefs(body)

	for _, child := range children {
		if err := w.replicate(ctx, idx, repo, "", string(child.Digest), child.MediaType, visited); err != nil {
			return fmt.Errorf("child %s: %w", child.Digest, err)
		}
	}

	for _, b := range blobs {
		if err := w.ensureBlob(ctx, client, destRepo, repo, b); err != nil {
			return fmt.Errorf("blob %s: %w", b.Digest, err)
		}
	}

	if err := client.PutManifest(ctx, destRepo, digest, mediaType, body); err != nil {
		return err
	}
	if tag != "" {
		if err := client.PutManifest(ctx, destRepo, tag, mediaType, body); err != nil {
			return err
		}
	}
	return nil
}

func (w *Worker) readManifest(ctx context.Context, repo, digest string) ([]byte, error) {
	rc, err := w.source.GetManifest(ctx, repo, ociregistry.Digest(digest))
	if err != nil {
		return nil, fmt.Errorf("reading manifest: %w", err)
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}

func (w *Worker) ensureBlob(ctx context.Context, client *Client, destRepo, srcRepo string, d ocispec.Descriptor) error {
	exists, err := client.BlobExists(ctx, destRepo, string(d.Digest))
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	rc, err := w.source.GetBlob(ctx, srcRepo, d.Digest)
	if err != nil {
		return fmt.Errorf("reading blob: %w", err)
	}
	defer func() { _ = rc.Close() }()
	size := d.Size
	if size == 0 {
		size = rc.Descriptor().Size
	}
	return client.PushBlob(ctx, destRepo, string(d.Digest), size, rc)
}

// Status returns a snapshot of each target's replication activity.
func (w *Worker) Status() []TargetStatus {
	out := make([]TargetStatus, len(w.targets))
	for i, t := range w.targets {
		st := w.states[i]
		st.mu.Lock()
		ts := TargetStatus{Name: t.Name, LastError: st.lastError, Replicated: st.replicated, Failed: st.failed, Dropped: st.dropped}
		if st.lastRunNS > 0 {
			last := time.Unix(0, st.lastRunNS)
			ts.LastRun = &last
		}
		st.mu.Unlock()
		out[i] = ts
	}
	return out
}

// parseRefs splits a manifest's referenced descriptors into blobs (config +
// layers) and child manifests (index entries).
func parseRefs(body []byte) (blobs, children []ocispec.Descriptor) {
	var m struct {
		Config    ocispec.Descriptor   `json:"config"`
		Layers    []ocispec.Descriptor `json:"layers"`
		Manifests []ocispec.Descriptor `json:"manifests"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, nil
	}
	if m.Config.Digest != "" {
		blobs = append(blobs, m.Config)
	}
	blobs = append(blobs, m.Layers...)
	return blobs, m.Manifests
}

package activitypub

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"sync"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/metrics"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/util"
)

// pubContext lets enqueueToFollowers apply per-follower tag-glob filters.
type pubContext struct {
	kind string
	repo string
	tag  string
}

const (
	pubKindManifest       = "manifest"
	pubKindTag            = "tag"
	pubKindBlob           = "blob"
	pubKindManifestDelete = "manifest-delete"
	pubKindTagDelete      = "tag-delete"
)

// PublicCollection is the special ActivityStreams address for public content.
const PublicCollection = "https://www.w3.org/ns/activitystreams#Public"

const followerBatchSize = 100

type PublisherRepository interface {
	PutActivity(ctx context.Context, activityID, activityType, actorURL string, activityJSON []byte) error
	ListFollowsBatch(ctx context.Context, afterID int64, limit int) ([]database.Actor, error)
	EnqueueDelivery(ctx context.Context, activityID, inboxURL string, activityJSON []byte) error
}

type APPublisher struct {
	identity      *Identity
	db            PublisherRepository
	actorCache    *ActorCache
	endpoint      string
	excludedRepos []string
	logger        *slog.Logger
	onEnqueue     func()
}

func (p *APPublisher) SetNotifyFunc(fn func()) {
	p.onEnqueue = fn
}

func NewAPPublisher(identity *Identity, db PublisherRepository, endpoint string, excludedRepos []string, logger *slog.Logger) *APPublisher {
	return &APPublisher{
		identity:      identity,
		db:            db,
		actorCache:    NewActorCache(identity),
		endpoint:      endpoint,
		excludedRepos: excludedRepos,
		logger:        logger,
	}
}

func (p *APPublisher) PublishManifest(ctx context.Context, repo, tag, digest, mediaType string, size int64, content []byte, subjectDigest *string) error {
	objectID := p.objectURL("manifest", digest)

	object := OCIManifest{
		Context:      ociContext(),
		Type:         TypeOCIManifest,
		ID:           objectID,
		AttributedTo: p.identity.ActorURL,
		Published:    NowRFC3339(),
		Repository:   repo,
		Digest:       digest,
		MediaType:    mediaType,
		Size:         size,
		Content:      EncodeContent(content),
		Tag:          tag,
	}
	if subjectDigest != nil {
		object.SubjectDigest = *subjectDigest
	}

	return p.createAndDeliver(ctx, ActivityCreate, object, pubContext{kind: pubKindManifest, repo: repo, tag: tag})
}

func (p *APPublisher) PublishTag(ctx context.Context, repo, tag, digest string) error {
	objectID := p.objectURL("tag", repo+"/"+tag)

	object := OCITag{
		Context:      ociContext(),
		Type:         TypeOCITag,
		ID:           objectID,
		AttributedTo: p.identity.ActorURL,
		Published:    NowRFC3339(),
		Repository:   repo,
		Tag:          tag,
		Digest:       digest,
	}

	return p.createAndDeliver(ctx, ActivityUpdate, object, pubContext{kind: pubKindTag, repo: repo, tag: tag})
}

// Publish is used by non-OCI backends (npm/cargo/pypi) which carry no tag context.
func (p *APPublisher) Publish(ctx context.Context, activityType string, object any) error {
	return p.createAndDeliver(ctx, activityType, object, pubContext{kind: pubKindBlob})
}

func (p *APPublisher) PublishManifestDelete(ctx context.Context, repo, digest string) error {
	objectID := p.objectURL("manifest", digest)

	object := OCIManifest{
		Context:      ociContext(),
		Type:         TypeOCIManifest,
		ID:           objectID,
		AttributedTo: p.identity.ActorURL,
		Published:    NowRFC3339(),
		Repository:   repo,
		Digest:       digest,
	}

	return p.createAndDeliver(ctx, ActivityDelete, object, pubContext{kind: pubKindManifestDelete, repo: repo})
}

func (p *APPublisher) PublishTagDelete(ctx context.Context, repo, tag string) error {
	objectID := p.objectURL("tag", repo+"/"+tag)

	object := OCITag{
		Context:      ociContext(),
		Type:         TypeOCITag,
		ID:           objectID,
		AttributedTo: p.identity.ActorURL,
		Published:    NowRFC3339(),
		Repository:   repo,
		Tag:          tag,
	}

	return p.createAndDeliver(ctx, ActivityDelete, object, pubContext{kind: pubKindTagDelete, repo: repo, tag: tag})
}

func (p *APPublisher) WithdrawRepo(ctx context.Context, repo string, tags []string, manifestDigests []string) error {
	for _, tag := range tags {
		if err := p.PublishTagDelete(ctx, repo, tag); err != nil {
			return fmt.Errorf("publishing tag delete %s:%s: %w", repo, tag, err)
		}
	}
	for _, dgst := range manifestDigests {
		if err := p.PublishManifestDelete(ctx, repo, dgst); err != nil {
			return fmt.Errorf("publishing manifest delete %s@%s: %w", repo, dgst, err)
		}
	}
	return nil
}

func (p *APPublisher) PublishBlobRef(ctx context.Context, digest string, size int64) error {
	objectID := p.objectURL("blob", digest)

	object := OCIBlob{
		Context:      ociContext(),
		Type:         TypeOCIBlob,
		ID:           objectID,
		AttributedTo: p.identity.ActorURL,
		Published:    NowRFC3339(),
		Digest:       digest,
		Size:         size,
		Endpoint:     p.endpoint,
	}

	return p.createAndDeliver(ctx, ActivityAnnounce, object, pubContext{kind: pubKindBlob})
}

// Stop releases background resources (actor cache eviction).
func (p *APPublisher) Stop() {
	p.actorCache.Stop()
}

func (p *APPublisher) ActorCache() *ActorCache {
	return p.actorCache
}

func (p *APPublisher) createAndDeliver(ctx context.Context, activityType string, object any, pubCtx pubContext) error {
	// Delete bypasses the exclusion filter so a repo added to excludedRepos
	// can still be withdrawn from peers via WithdrawRepo.
	if activityType != ActivityDelete && p.repoExcluded(pubCtx.repo) {
		p.logger.Debug("publisher: repo excluded from outbound federation", "repo", pubCtx.repo, "activityType", activityType)
		return nil
	}
	metrics.OutboundActivities.WithLabelValues(activityType).Inc()
	activityID := p.activityURL()
	followersURL := p.endpoint + "/ap/followers"
	p.logger.Debug("publisher: createAndDeliver", "activityType", activityType, "activityID", activityID)

	activity := map[string]any{
		KeyContext: ContextActivityStreams,
		KeyID:      activityID,
		KeyType:    activityType,
		KeyActor:   p.identity.ActorURL,
		"to":       []string{PublicCollection},
		"cc":       []string{followersURL},
		KeyObject:  object,
	}

	activityJSON, err := json.Marshal(activity)
	if err != nil {
		return fmt.Errorf("marshaling activity: %w", err)
	}

	if err := p.db.PutActivity(ctx, activityID, activityType, p.identity.ActorURL, activityJSON); err != nil {
		return fmt.Errorf("storing activity: %w", err)
	}

	return p.enqueueToFollowers(ctx, activityID, activityJSON, pubCtx)
}

// repoExcluded reports whether outbound federation should drop activities for repo.
// Activities without a repo (npm/cargo/pypi via Publish, OCI blob announces) always
// pass since the filter is repo-scoped.
func (p *APPublisher) repoExcluded(repo string) bool {
	if repo == "" {
		return false
	}
	for _, g := range p.excludedRepos {
		if util.MatchRepoGlob(g, repo, p.identity.AccountDomain) {
			return true
		}
	}
	return false
}

// actorAcceptsActivity matches a follower's federation_tag_globs filter against
// the activity. Activities without a tag context (blobs, manifest deletes, and
// manifests pushed by digest) always pass — otherwise a later tag activity
// would point at content the peer never received.
func actorAcceptsActivity(actor *database.Actor, pubCtx pubContext) bool {
	if pubCtx.kind == pubKindBlob || pubCtx.kind == pubKindManifestDelete {
		return true
	}
	if pubCtx.kind == pubKindManifest && pubCtx.tag == "" {
		return true
	}
	if actor.FederationTagGlobs == nil {
		return true
	}
	raw := strings.TrimSpace(*actor.FederationTagGlobs)
	if raw == "" {
		// Explicit empty list = receive nothing tag-bearing.
		return false
	}
	for glob := range strings.SplitSeq(raw, ",") {
		g := strings.TrimSpace(glob)
		if g == "" {
			continue
		}
		if ok, err := path.Match(g, pubCtx.tag); err == nil && ok {
			return true
		}
	}
	return false
}

// enqueueToFollowers resolves follower inboxes (using shared inbox when available)
// and enqueues deliveries to the persistent delivery queue.
// Followers are loaded in batches to avoid unbounded memory usage.
func (p *APPublisher) enqueueToFollowers(ctx context.Context, activityID string, activityJSON []byte, pubCtx pubContext) error {
	var (
		mu      sync.Mutex
		inboxes = make(map[string]struct{})
	)

	var afterID int64
	for {
		batch, err := p.db.ListFollowsBatch(ctx, afterID, followerBatchSize)
		if err != nil {
			return fmt.Errorf("listing followers: %w", err)
		}
		if len(batch) == 0 {
			break
		}

		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(20)
		for _, f := range batch {
			if !actorAcceptsActivity(&f, pubCtx) {
				continue
			}
			g.Go(func() error {
				inbox, err := p.resolveInbox(gctx, f.ActorURL)
				if err != nil {
					p.logger.Warn("failed to resolve inbox for follower", "actor", f.ActorURL, "error", err)
					return nil
				}
				mu.Lock()
				inboxes[inbox] = struct{}{}
				mu.Unlock()
				return nil
			})
		}
		if err := g.Wait(); err != nil {
			return err
		}

		afterID = batch[len(batch)-1].ID
		if len(batch) < followerBatchSize {
			break
		}
	}

	p.logger.Debug("publisher: resolved inboxes", "activityID", activityID, "count", len(inboxes))
	for inbox := range inboxes {
		if err := p.db.EnqueueDelivery(ctx, activityID, inbox, activityJSON); err != nil {
			p.logger.Error("failed to enqueue delivery", "inbox", inbox, "error", err)
		} else {
			metrics.DeliveryEnqueued.Add(1)
			p.logger.Debug("publisher: enqueued delivery", "activityID", activityID, "inbox", inbox)
		}
	}

	if len(inboxes) > 0 && p.onEnqueue != nil {
		p.onEnqueue()
	}

	return nil
}

// resolveInbox returns the shared inbox if available, otherwise the actor's personal inbox.
func (p *APPublisher) resolveInbox(ctx context.Context, actorURL string) (string, error) {
	actor, err := p.actorCache.Get(ctx, actorURL)
	if err != nil {
		return "", err
	}

	if sharedInbox, ok := actor.Endpoints["sharedInbox"]; ok && sharedInbox != "" {
		return sharedInbox, nil
	}

	return actor.Inbox, nil
}

func (p *APPublisher) objectURL(kind, ref string) string {
	sanitized := strings.ReplaceAll(ref, "/", ":")
	return p.endpoint + "/ap/objects/" + kind + "/" + sanitized
}

func (p *APPublisher) activityURL() string {
	return p.endpoint + "/ap/activities/" + uuid.New().String()
}

package nuget

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/activitypub"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/blobstore"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/registry/pkgfed"
)

const (
	packageType = "nuget"
	routePrefix = "/nuget"
)

type Backend struct {
	db         *database.DB
	blobs      blobstore.BlobStore
	logger     *slog.Logger
	endpoint   string
	token      string
	owner      string
	publisher  activitypub.PackagePublisher
	replicator pkgfed.Replicator
	handler    http.Handler
}

type Config struct {
	DB         *database.DB
	Blobs      blobstore.BlobStore
	Endpoint   string
	Token      string
	Owner      string
	Publisher  activitypub.PackagePublisher
	Replicator pkgfed.Replicator
	Logger     *slog.Logger
}

func New(cfg Config) *Backend {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	b := &Backend{
		db:         cfg.DB,
		blobs:      cfg.Blobs,
		logger:     cfg.Logger,
		endpoint:   strings.TrimRight(cfg.Endpoint, "/"),
		token:      cfg.Token,
		owner:      cfg.Owner,
		publisher:  cfg.Publisher,
		replicator: cfg.Replicator,
	}
	b.handler = b.routes()
	return b
}

func (b *Backend) Type() string          { return packageType }
func (b *Backend) RoutePrefix() string   { return routePrefix }
func (b *Backend) Handler() http.Handler { return b.handler }

func (b *Backend) routes() http.Handler {
	r := chi.NewRouter()

	r.Get("/v3/index.json", b.handleServiceIndex)

	r.With(b.requireToken).Put("/v3/package", b.handlePush)
	r.With(b.requireToken).Delete("/v3/package/{id}/{version}", b.handleDelete)

	r.Get("/v3-flatcontainer/{id}/index.json", b.handleVersionList)
	r.Get("/v3-flatcontainer/{id}/{version}/{filename}", b.handleDownload)

	r.Get("/v3/registration/{id}/index.json", b.handleRegistrationIndex)
	// {slug} captures the whole segment e.g. "1.0.0.json"; handler strips ".json".
	r.Get("/v3/registration/{id}/{slug}", b.handleRegistrationLeaf)

	return http.StripPrefix(routePrefix, r)
}

// NuGet clients send their API key in X-NuGet-ApiKey (not Bearer/Basic).
func (b *Backend) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if b.token == "" {
			next.ServeHTTP(w, r)
			return
		}
		key := r.Header.Get("X-NuGet-ApiKey")
		if subtle.ConstantTimeCompare([]byte(key), []byte(b.token)) != 1 {
			w.Header().Set("WWW-Authenticate", `ApiKey realm="apoci"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func normalizeID(id string) string {
	return strings.ToLower(id)
}

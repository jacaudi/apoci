package cargo

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/activitypub"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/blobstore"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
)

const (
	packageType = "cargo"
	routePrefix = "/cargo"
)

type Backend struct {
	db        *database.DB
	blobs     blobstore.BlobStore
	logger    *slog.Logger
	endpoint  string
	token     string
	owner     string
	publisher activitypub.PackagePublisher
	handler   http.Handler
}

type Config struct {
	DB        *database.DB
	Blobs     blobstore.BlobStore
	Endpoint  string
	Token     string
	Owner     string
	Publisher activitypub.PackagePublisher
	Logger    *slog.Logger
}

func New(cfg Config) *Backend {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	b := &Backend{
		db:        cfg.DB,
		blobs:     cfg.Blobs,
		logger:    cfg.Logger,
		endpoint:  strings.TrimRight(cfg.Endpoint, "/"),
		token:     cfg.Token,
		owner:     cfg.Owner,
		publisher: cfg.Publisher,
	}
	b.handler = b.routes()
	return b
}

func (b *Backend) Type() string          { return packageType }
func (b *Backend) RoutePrefix() string   { return routePrefix }
func (b *Backend) Handler() http.Handler { return b.handler }

func (b *Backend) routes() http.Handler {
	r := chi.NewRouter()

	r.Get("/config.json", b.handleConfig)

	r.With(b.requireToken).Put("/api/v1/crates/new", b.handlePublish)
	r.Get("/api/v1/crates/{name}/{version}/download", b.handleDownload)
	r.With(b.requireToken).Delete("/api/v1/crates/{name}/{version}/yank", b.handleYank)
	r.With(b.requireToken).Put("/api/v1/crates/{name}/{version}/unyank", b.handleUnyank)

	r.Get("/1/{name}", b.handleIndex)
	r.Get("/2/{name}", b.handleIndex)
	r.Get("/3/{first}/{name}", b.handleIndex)
	r.Get("/{first2}/{next2}/{name}", b.handleIndex)

	return http.StripPrefix(routePrefix, r)
}

// Cargo sends the raw token in Authorization, no "Bearer " prefix.
func (b *Backend) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if b.token == "" {
			next.ServeHTTP(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		if subtle.ConstantTimeCompare([]byte(auth), []byte(b.token)) != 1 {
			http.Error(w, `{"errors":[{"detail":"unauthorized"}]}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

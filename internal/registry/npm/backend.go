package npm

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/blobstore"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
)

const (
	packageType = "npm"
	routePrefix = "/npm"
)

type Backend struct {
	db       *database.DB
	blobs    blobstore.BlobStore
	logger   *slog.Logger
	endpoint string
	token    string
	owner    string
	handler  http.Handler
}

type Config struct {
	DB       *database.DB
	Blobs    blobstore.BlobStore
	Endpoint string
	Token    string
	Owner    string
	Logger   *slog.Logger
}

func New(cfg Config) *Backend {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	b := &Backend{
		db:       cfg.DB,
		blobs:    cfg.Blobs,
		logger:   cfg.Logger,
		endpoint: strings.TrimRight(cfg.Endpoint, "/"),
		token:    cfg.Token,
		owner:    cfg.Owner,
	}
	b.handler = b.routes()
	return b
}

func (b *Backend) Type() string          { return packageType }
func (b *Backend) RoutePrefix() string   { return routePrefix }
func (b *Backend) Handler() http.Handler { return b.handler }

func (b *Backend) routes() http.Handler {
	r := chi.NewRouter()

	r.Get("/-/package/{name}/dist-tags", b.handleDistTagsList)
	r.Get("/-/package/@{scope}/{name}/dist-tags", b.handleDistTagsList)
	r.With(b.requireToken).Put("/-/package/{name}/dist-tags/{tag}", b.handleDistTagPut)
	r.With(b.requireToken).Put("/-/package/@{scope}/{name}/dist-tags/{tag}", b.handleDistTagPut)
	r.With(b.requireToken).Delete("/-/package/{name}/dist-tags/{tag}", b.handleDistTagDelete)
	r.With(b.requireToken).Delete("/-/package/@{scope}/{name}/dist-tags/{tag}", b.handleDistTagDelete)

	r.Get("/{name}/-/{tarball}", b.handleTarball)
	r.Get("/@{scope}/{name}/-/{tarball}", b.handleTarball)
	r.Head("/{name}/-/{tarball}", b.handleTarball)
	r.Head("/@{scope}/{name}/-/{tarball}", b.handleTarball)

	r.Get("/{name}", b.handlePackument)
	r.Get("/@{scope}/{name}", b.handlePackument)

	r.With(b.requireToken).Put("/{name}", b.handlePublish)
	r.With(b.requireToken).Put("/@{scope}/{name}", b.handlePublish)

	return http.StripPrefix(routePrefix, r)
}

func (b *Backend) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if b.token == "" {
			next.ServeHTTP(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		const bearer = "Bearer "
		if !strings.HasPrefix(auth, bearer) ||
			subtle.ConstantTimeCompare([]byte(auth[len(bearer):]), []byte(b.token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="apoci"`)
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// packageNameFromURL: PUT publishes arrive URL-encoded as `/@scope%2Fname`,
// so chi captures the whole thing as a single {name} param; unescape here.
func packageNameFromURL(r *http.Request) string {
	scope := chi.URLParam(r, "scope")
	name := chi.URLParam(r, "name")
	if scope != "" {
		return "@" + scope + "/" + name
	}
	if decoded, err := url.PathUnescape(name); err == nil {
		return decoded
	}
	return name
}

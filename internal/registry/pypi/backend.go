package pypi

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/activitypub"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/blobstore"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/registry/pkgfed"
)

const (
	packageType = "pypi"
	routePrefix = "/pypi"
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

	r.With(b.requireToken).Post("/", b.handleUpload)

	r.Get("/simple/", b.handleSimpleRoot)
	r.Get("/simple/{name}/", b.handleSimplePackage)
	r.Get("/simple/{name}", b.handleSimplePackage)

	r.Get("/files/{name}/{version}/{filename}", b.handleDownload)

	return http.StripPrefix(routePrefix, r)
}

// twine sends Basic auth with username "__token__"; pip and clients-with-tokens send Bearer.
func (b *Backend) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if b.token == "" {
			next.ServeHTTP(w, r)
			return
		}
		if !checkPyPIAuth(r, b.token) {
			w.Header().Set("WWW-Authenticate", `Basic realm="apoci"`)
			http.Error(w, `unauthorized`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func checkPyPIAuth(r *http.Request, token string) bool {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return subtle.ConstantTimeCompare([]byte(auth[len("Bearer "):]), []byte(token)) == 1
	}
	if _, password, ok := r.BasicAuth(); ok {
		return subtle.ConstantTimeCompare([]byte(password), []byte(token)) == 1
	}
	return false
}

// PEP 503: lowercase, collapse runs of [-_.] to a single hyphen.
var pep503Run = regexp.MustCompile(`[-_.]+`)

func normalizeName(name string) string {
	return pep503Run.ReplaceAllString(strings.ToLower(name), "-")
}

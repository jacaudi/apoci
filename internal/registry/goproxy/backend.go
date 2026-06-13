// Package goproxy implements the Go module proxy protocol as an apoci backend.
// It serves modules both as a store (private modules pushed in via an authed
// PUT, owned and federated) and as a pull-through cache of upstream proxies
// such as proxy.golang.org.
package goproxy

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"

	"golang.org/x/mod/module"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/activitypub"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/blobstore"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/registry/pkgfed"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/upstream"
)

const (
	packageType = "goproxy"
	routePrefix = "/goproxy"

	// upstreamOwner marks packages cached from an upstream proxy, keeping them
	// distinct from locally-published (node-owned) modules.
	upstreamOwner = "upstream:goproxy"

	zipMediaType = "application/zip"
	modMediaType = "text/plain; charset=utf-8"
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
	upstream   *upstream.GoFetcher
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
	Upstream   *upstream.GoFetcher
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
		upstream:   cfg.Upstream,
	}
	b.handler = http.StripPrefix(routePrefix, http.HandlerFunc(b.dispatch))
	return b
}

func (b *Backend) Type() string          { return packageType }
func (b *Backend) RoutePrefix() string   { return routePrefix }
func (b *Backend) Handler() http.Handler { return b.handler }

// dispatch parses the GOPROXY-protocol path. Module paths contain slashes and
// the "/@v/" and "/@latest" delimiters sit inside the path, so we split
// manually rather than using per-segment routing params.
func (b *Backend) dispatch(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/")

	if escMod, ok := strings.CutSuffix(p, "/@latest"); ok {
		mod, err := module.UnescapePath(escMod)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid module path")
			return
		}
		b.requireGET(w, r, func() { b.handleLatest(w, r, mod) })
		return
	}

	escMod, rest, found := strings.Cut(p, "/@v/")
	if !found {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	mod, err := module.UnescapePath(escMod)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid module path")
		return
	}

	switch {
	case rest == "list":
		b.requireGET(w, r, func() { b.handleList(w, r, mod) })
	case strings.HasSuffix(rest, ".info"):
		b.withVersion(w, r, rest, ".info", func(ver string) { b.handleInfo(w, r, mod, ver) })
	case strings.HasSuffix(rest, ".mod"):
		b.withVersion(w, r, rest, ".mod", func(ver string) { b.handleMod(w, r, mod, ver) })
	case strings.HasSuffix(rest, ".zip"):
		ver, ok := decodeVersion(w, rest, ".zip")
		if !ok {
			return
		}
		switch r.Method {
		case http.MethodGet:
			b.handleZip(w, r, mod, ver)
		case http.MethodPut:
			b.requireToken(w, r, func() { b.handleUpload(w, r, mod, ver) })
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

// withVersion decodes the bang-escaped version from "<ver><suffix>" and runs fn
// for GET requests only.
func (b *Backend) withVersion(w http.ResponseWriter, r *http.Request, rest, suffix string, fn func(ver string)) {
	ver, ok := decodeVersion(w, rest, suffix)
	if !ok {
		return
	}
	b.requireGET(w, r, func() { fn(ver) })
}

func decodeVersion(w http.ResponseWriter, rest, suffix string) (string, bool) {
	ver, err := module.UnescapeVersion(strings.TrimSuffix(rest, suffix))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid version")
		return "", false
	}
	return ver, true
}

func (b *Backend) requireGET(w http.ResponseWriter, r *http.Request, fn func()) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	fn()
}

// requireToken authenticates uploads. Go clients have no native publish, so a
// CI/upload step sends the token via Bearer or Basic (password) auth.
func (b *Backend) requireToken(w http.ResponseWriter, r *http.Request, fn func()) {
	if b.token == "" {
		fn()
		return
	}
	if tokenMatches(r, b.token) {
		fn()
		return
	}
	w.Header().Set("WWW-Authenticate", `Bearer realm="apoci"`)
	writeError(w, http.StatusUnauthorized, "unauthorized")
}

func tokenMatches(r *http.Request, token string) bool {
	if bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(bearer)), []byte(token)) == 1 {
			return true
		}
	}
	if _, pass, ok := r.BasicAuth(); ok {
		if subtle.ConstantTimeCompare([]byte(pass), []byte(token)) == 1 {
			return true
		}
	}
	return false
}

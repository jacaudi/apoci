package goproxy

import (
	"context"
	"fmt"
	"net/http"

	"golang.org/x/mod/module"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/activitypub"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/registry/pkgfed"
)

const apTypeModule = "GoModule"

// GoModule is the ActivityPub object federated for a Go module version.
type GoModule struct {
	Context    []string `json:"@context"`
	Type       string   `json:"type"`
	ID         string   `json:"id"`
	Published  string   `json:"published"`
	GoMod      string   `json:"goModule"`
	GoVersion  string   `json:"goVersion"`
	GoFile     string   `json:"goFile"`
	GoBlobSHA  string   `json:"goBlobSHA256"`
	GoSize     int64    `json:"goSize"`
	GoURL      string   `json:"goURL"`
	GoInfoMeta []byte   `json:"goInfoMeta,omitempty"`
}

type federationAdapter struct {
	backend *Backend
}

func (b *Backend) FederationAdapter() activitypub.FederationAdapter {
	return &federationAdapter{backend: b}
}

func (a *federationAdapter) PackageType() string { return packageType }
func (a *federationAdapter) APTypes() []string   { return []string{apTypeModule} }

func (a *federationAdapter) Ingest(ctx context.Context, _, apType string, obj map[string]any, actorURL string) error {
	if apType != apTypeModule {
		return nil
	}
	return a.ingestModule(ctx, obj, actorURL)
}

func (a *federationAdapter) ingestModule(ctx context.Context, obj map[string]any, actorURL string) error {
	mod, _ := obj["goModule"].(string)
	ver, _ := obj["goVersion"].(string)
	filename, _ := obj["goFile"].(string)
	if mod == "" || ver == "" || filename == "" {
		return fmt.Errorf("go module: missing module, version, or filename")
	}

	dbPkg, err := a.backend.db.GetOrCreatePackage(ctx, packageType, mod, actorURL)
	if err != nil {
		return fmt.Errorf("go module: get-or-create: %w", err)
	}

	v, err := a.backend.db.GetPackageVersion(ctx, dbPkg.ID, ver)
	if err != nil {
		return fmt.Errorf("go module: lookup version: %w", err)
	}
	if v == nil {
		var infoRaw []byte
		if meta, ok := obj["goInfoMeta"].(string); ok && meta != "" {
			infoRaw = []byte(meta)
		}
		v = &database.PackageVersion{
			PackageID:   dbPkg.ID,
			Version:     ver,
			Metadata:    infoRaw,
			MediaType:   versionMediaType,
			SizeBytes:   int64(len(infoRaw)),
			SourceActor: &actorURL,
		}
		if err := a.backend.db.PutPackageVersion(ctx, v); err != nil {
			return fmt.Errorf("go module: put version: %w", err)
		}
	}

	blobSHA, _ := obj["goBlobSHA256"].(string)
	size, _ := obj["goSize"].(float64)
	ct := zipMediaType
	if blobSHA != "" {
		if err := a.backend.db.PutBlob(ctx, blobSHA, int64(size), &ct, false); err != nil {
			return fmt.Errorf("go module: put blob ref: %w", err)
		}
		if err := pkgfed.RecordPeerBlob(ctx, a.backend.db, actorURL, blobSHA); err != nil {
			return fmt.Errorf("go module: put peer blob: %w", err)
		}
		if a.backend.replicator != nil {
			if peer := activitypub.EndpointFromActorURL(actorURL); peer != "" {
				a.backend.replicator.ReplicateFromURL(ctx, peerFileURL(peer, mod, ver, ".zip"), blobSHA)
			}
		}
	}

	file := &database.PackageFile{
		VersionID:   v.ID,
		Filename:    ver + ".zip",
		BlobDigest:  blobSHA,
		SizeBytes:   int64(size),
		ContentType: &ct,
	}
	if err := a.backend.db.PutPackageFile(ctx, file); err != nil {
		return fmt.Errorf("go module: put file: %w", err)
	}
	return nil
}

func (b *Backend) publishModule(ctx context.Context, mod, ver string, zipFile *database.PackageFile, infoRaw []byte) {
	if b.publisher == nil {
		return
	}
	obj := GoModule{
		Context:    activitypub.BaseContext(),
		Type:       apTypeModule,
		ID:         b.endpoint + "/ap/objects/go-module/" + escapePathOrEmpty(mod) + "/" + escapeVersionOrEmpty(ver),
		Published:  activitypub.NowRFC3339(),
		GoMod:      mod,
		GoVersion:  ver,
		GoFile:     zipFile.Filename,
		GoBlobSHA:  zipFile.BlobDigest,
		GoSize:     zipFile.SizeBytes,
		GoURL:      b.moduleURL(mod, ver, ".zip"),
		GoInfoMeta: infoRaw,
	}
	if err := b.publisher.Publish(ctx, "Create", obj); err != nil {
		b.logger.Warn("goproxy federation: publish module", "err", err, "module", mod, "version", ver)
	}
}

func (b *Backend) redirectToPeer(ctx context.Context, w http.ResponseWriter, r *http.Request, digest, mod, ver, suffix string) bool {
	return pkgfed.RedirectToPeer(ctx, w, r, b.db, digest, func(peer string) string {
		return peerFileURL(peer, mod, ver, suffix)
	})
}

func (b *Backend) moduleURL(mod, ver, suffix string) string {
	return b.endpoint + peerPath(mod, ver, suffix)
}

func peerFileURL(peerEndpoint, mod, ver, suffix string) string {
	return peerEndpoint + peerPath(mod, ver, suffix)
}

// peerPath builds the GOPROXY download path for a module version artifact,
// e.g. "/goproxy/github.com/foo/bar/@v/v1.0.0.zip" (bang-escaped).
func peerPath(mod, ver, suffix string) string {
	return routePrefix + "/" + escapePathOrEmpty(mod) + "/@v/" + escapeVersionOrEmpty(ver) + suffix
}

func escapePathOrEmpty(mod string) string {
	if esc, err := module.EscapePath(mod); err == nil {
		return esc
	}
	return mod
}

func escapeVersionOrEmpty(ver string) string {
	if esc, err := module.EscapeVersion(ver); err == nil {
		return esc
	}
	return ver
}

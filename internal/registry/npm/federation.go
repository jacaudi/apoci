package npm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/activitypub"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/registry/pkgfed"
)

const (
	apTypeVersion = "NpmVersion"
	apTypeTag     = "NpmTag"
)

// NpmVersion: peers store metadata; tarball requests are 302'd back to a peer
// that holds the blob. npmMeta is the per-version package.json fragment.
type NpmVersion struct {
	Context      []string        `json:"@context"`
	Type         string          `json:"type"`
	ID           string          `json:"id"`
	Published    string          `json:"published"`
	NpmName      string          `json:"npmName"`
	NpmVersion   string          `json:"npmVersion"`
	NpmIntegrity string          `json:"npmIntegrity"`
	NpmShasum    string          `json:"npmShasum"`
	NpmTarball   string          `json:"npmTarball"`
	NpmFilename  string          `json:"npmFilename"`
	NpmBlobSHA   string          `json:"npmBlobSHA256"`
	NpmSize      int64           `json:"npmSize"`
	NpmMeta      json.RawMessage `json:"npmMeta"`
}

type NpmTag struct {
	Context    []string `json:"@context"`
	Type       string   `json:"type"`
	ID         string   `json:"id"`
	Published  string   `json:"published"`
	NpmName    string   `json:"npmName"`
	NpmTag     string   `json:"npmTag"`
	NpmVersion string   `json:"npmVersion,omitempty"`
}

type federationAdapter struct {
	backend *Backend
}

func (b *Backend) FederationAdapter() activitypub.FederationAdapter {
	return &federationAdapter{backend: b}
}

func (a *federationAdapter) PackageType() string { return packageType }
func (a *federationAdapter) APTypes() []string   { return []string{apTypeVersion, apTypeTag} }

func (a *federationAdapter) Ingest(ctx context.Context, activityType, apType string, obj map[string]any, actorURL string) error {
	switch apType {
	case apTypeVersion:
		if activityType == "Delete" {
			return a.deleteVersion(ctx, obj, actorURL)
		}
		return a.ingestVersion(ctx, obj, actorURL)
	case apTypeTag:
		if activityType == "Delete" {
			return a.deleteTag(ctx, obj, actorURL)
		}
		return a.upsertTag(ctx, obj, actorURL)
	}
	return nil
}

func (a *federationAdapter) ingestVersion(ctx context.Context, obj map[string]any, actorURL string) error {
	name, _ := obj["npmName"].(string)
	version, _ := obj["npmVersion"].(string)
	if name == "" || version == "" {
		return fmt.Errorf("npm version: missing npmName or npmVersion")
	}

	dbPkg, err := a.backend.db.GetOrCreatePackage(ctx, packageType, name, actorURL)
	if err != nil {
		return fmt.Errorf("npm version: get-or-create: %w", err)
	}

	stored := storedVersion{}
	if integrity, ok := obj["npmIntegrity"].(string); ok {
		stored.Integrity = integrity
	}
	if shasum, ok := obj["npmShasum"].(string); ok {
		stored.Shasum = shasum
	}
	if meta, ok := obj["npmMeta"]; ok && meta != nil {
		if err := pkgfed.RemarshalInto(meta, &stored.Meta); err != nil {
			return fmt.Errorf("npm version: decode meta: %w", err)
		}
	}
	metadataBytes, err := json.Marshal(stored)
	if err != nil {
		return fmt.Errorf("npm version: encode stored: %w", err)
	}

	v := &database.PackageVersion{
		PackageID:   dbPkg.ID,
		Version:     version,
		Metadata:    metadataBytes,
		MediaType:   versionMediaType,
		SizeBytes:   int64(len(metadataBytes)),
		SourceActor: &actorURL,
	}
	if err := a.backend.db.PutPackageVersion(ctx, v); err != nil {
		return fmt.Errorf("npm version: put version: %w", err)
	}

	if filename, _ := obj["npmFilename"].(string); filename != "" {
		blobDigest, _ := obj["npmBlobSHA256"].(string)
		size, _ := obj["npmSize"].(float64)
		contentType := tarballMediaType
		if blobDigest != "" {
			if err := a.backend.db.PutBlob(ctx, blobDigest, int64(size), &contentType, false); err != nil {
				return fmt.Errorf("npm version: put blob ref: %w", err)
			}
			if err := pkgfed.RecordPeerBlob(ctx, a.backend.db, actorURL, blobDigest); err != nil {
				return fmt.Errorf("npm version: put peer blob: %w", err)
			}
			if a.backend.replicator != nil {
				if peer := activitypub.EndpointFromActorURL(actorURL); peer != "" {
					a.backend.replicator.ReplicateFromURL(ctx, peerTarballURL(peer, name, filename), blobDigest)
				}
			}
		}
		file := &database.PackageFile{
			VersionID:   v.ID,
			Filename:    filename,
			BlobDigest:  blobDigest,
			SizeBytes:   int64(size),
			ContentType: &contentType,
		}
		if err := a.backend.db.PutPackageFile(ctx, file); err != nil {
			return fmt.Errorf("npm version: put file: %w", err)
		}
	}
	return nil
}

func (a *federationAdapter) deleteVersion(ctx context.Context, obj map[string]any, actorURL string) error {
	name, _ := obj["npmName"].(string)
	version, _ := obj["npmVersion"].(string)
	if name == "" || version == "" {
		return fmt.Errorf("npm delete: missing npmName or npmVersion")
	}
	dbPkg, err := pkgfed.LookupOwnedPackage(ctx, a.backend.db, packageType, name, actorURL)
	if err != nil || dbPkg == nil {
		return err
	}
	return a.backend.db.DeletePackageVersion(ctx, dbPkg.ID, version)
}

func (a *federationAdapter) upsertTag(ctx context.Context, obj map[string]any, actorURL string) error {
	name, _ := obj["npmName"].(string)
	tag, _ := obj["npmTag"].(string)
	version, _ := obj["npmVersion"].(string)
	if name == "" || tag == "" || version == "" {
		return fmt.Errorf("npm tag: missing fields")
	}
	dbPkg, err := pkgfed.LookupOwnedPackage(ctx, a.backend.db, packageType, name, actorURL)
	if err != nil || dbPkg == nil {
		return err
	}
	return a.backend.db.PutPackageTag(ctx, dbPkg.ID, tag, version, false, false)
}

func (a *federationAdapter) deleteTag(ctx context.Context, obj map[string]any, actorURL string) error {
	name, _ := obj["npmName"].(string)
	tag, _ := obj["npmTag"].(string)
	if name == "" || tag == "" {
		return fmt.Errorf("npm tag delete: missing fields")
	}
	dbPkg, err := pkgfed.LookupOwnedPackage(ctx, a.backend.db, packageType, name, actorURL)
	if err != nil || dbPkg == nil {
		return err
	}
	return a.backend.db.DeletePackageTag(ctx, dbPkg.ID, tag)
}

// Publish errors are logged, not returned: federation is best-effort and must
// not block the local write that already committed.
func (b *Backend) publishVersion(ctx context.Context, name string, v *database.PackageVersion, file *database.PackageFile, integrity, shasum string, meta any) {
	if b.publisher == nil {
		return
	}
	rawMeta, err := json.Marshal(meta)
	if err != nil {
		b.logger.Warn("npm federation: marshal meta", "err", err)
		return
	}
	obj := NpmVersion{
		Context:      activitypub.BaseContext(),
		Type:         apTypeVersion,
		ID:           b.endpoint + "/ap/objects/npm-version/" + url.PathEscape(name) + "/" + url.PathEscape(v.Version),
		Published:    activitypub.NowRFC3339(),
		NpmName:      name,
		NpmVersion:   v.Version,
		NpmIntegrity: integrity,
		NpmShasum:    shasum,
		NpmTarball:   b.tarballURL(name, file.Filename),
		NpmFilename:  file.Filename,
		NpmBlobSHA:   file.BlobDigest,
		NpmSize:      file.SizeBytes,
		NpmMeta:      rawMeta,
	}
	if err := b.publisher.Publish(ctx, "Create", obj); err != nil {
		b.logger.Warn("npm federation: publish version", "err", err, "name", name, "version", v.Version)
	}
}

func (b *Backend) publishTagSet(ctx context.Context, name, tag, version string) {
	b.publishTag(ctx, "Update", name, tag, version)
}

func (b *Backend) publishTagDelete(ctx context.Context, name, tag string) {
	b.publishTag(ctx, "Delete", name, tag, "")
}

func (b *Backend) publishTag(ctx context.Context, activityType, name, tag, version string) {
	if b.publisher == nil {
		return
	}
	obj := NpmTag{
		Context:    activitypub.BaseContext(),
		Type:       apTypeTag,
		ID:         b.endpoint + "/ap/objects/npm-tag/" + url.PathEscape(name) + "/" + url.PathEscape(tag),
		Published:  activitypub.NowRFC3339(),
		NpmName:    name,
		NpmTag:     tag,
		NpmVersion: version,
	}
	if err := b.publisher.Publish(ctx, activityType, obj); err != nil {
		b.logger.Warn("npm federation: publish tag", "err", err, "activity", activityType, "name", name, "tag", tag)
	}
}

func (b *Backend) redirectToPeer(ctx context.Context, w http.ResponseWriter, r *http.Request, digest, packageName, filename string) bool {
	peers, err := b.db.FindPeersWithBlob(ctx, digest)
	if err != nil || len(peers) == 0 {
		return false
	}
	http.Redirect(w, r, peerTarballURL(peers[0].PeerEndpoint, packageName, filename), http.StatusFound) //nolint:gosec // peer endpoint sourced from authenticated federation activity
	return true
}

// peerTarballURL builds a tarball URL on a peer. packageName is intentionally
// not url.PathEscape'd: scoped names ("@scope/foo") rely on the embedded slash
// being a path separator on the upstream.
func peerTarballURL(peerEndpoint, packageName, filename string) string {
	return peerEndpoint + routePrefix + "/" + packageName + "/-/" + url.PathEscape(filename)
}

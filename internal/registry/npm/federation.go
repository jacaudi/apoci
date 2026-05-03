package npm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/activitypub"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
)

const (
	apTypeVersion = "NpmVersion"
	apTypeTag     = "NpmTag"
)

// NpmVersion: tarball is fetched lazily from NpmTarball; npmMeta is the
// per-version package.json fragment.
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

func npmContext() []string {
	return []string{activitypub.ContextActivityStreams, activitypub.ContextSecurity}
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
		if err := remarshalInto(meta, &stored.Meta); err != nil {
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
	dbPkg, err := lookupOwnedPackage(ctx, a.backend.db, name, actorURL)
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
	dbPkg, err := lookupOwnedPackage(ctx, a.backend.db, name, actorURL)
	if err != nil || dbPkg == nil {
		return err
	}
	return a.backend.db.PutPackageTag(ctx, dbPkg.ID, tag, version, false)
}

func (a *federationAdapter) deleteTag(ctx context.Context, obj map[string]any, actorURL string) error {
	name, _ := obj["npmName"].(string)
	tag, _ := obj["npmTag"].(string)
	if name == "" || tag == "" {
		return fmt.Errorf("npm tag delete: missing fields")
	}
	dbPkg, err := lookupOwnedPackage(ctx, a.backend.db, name, actorURL)
	if err != nil || dbPkg == nil {
		return err
	}
	return a.backend.db.DeletePackageTag(ctx, dbPkg.ID, tag)
}

// publishVersion broadcasts on publish. Errors must not block the local write.
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
		Context:      npmContext(),
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
		Context:    npmContext(),
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

// lookupOwnedPackage returns nil,nil for an unknown package (delete-before-create
// is a no-op rather than an error) and an error if the sender doesn't own it.
func lookupOwnedPackage(ctx context.Context, db *database.DB, name, actorURL string) (*database.Package, error) {
	pkg, err := db.GetPackage(ctx, packageType, name)
	if err != nil {
		return nil, fmt.Errorf("lookup package: %w", err)
	}
	if pkg == nil {
		return nil, nil
	}
	if pkg.OwnerID != actorURL {
		return nil, fmt.Errorf("npm package %s not owned by %s", name, actorURL)
	}
	return pkg, nil
}

// remarshalInto round-trips a decoded JSON value through Marshal so it can be
// re-Unmarshaled into a typed target. Needed because map[string]any decoding
// loses the json.RawMessage form of nested objects.
func remarshalInto(v any, target any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, target)
}

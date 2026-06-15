package cargo

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
	apTypeVersion = "CargoVersion"
	apTypeYank    = "CargoYank"
)

// CargoVersion: peers store metadata; .crate requests are 302'd to a peer that
// holds the blob.
type CargoVersion struct {
	Context      []string        `json:"@context"`
	Type         string          `json:"type"`
	ID           string          `json:"id"`
	Published    string          `json:"published"`
	CargoName    string          `json:"cargoName"`
	CargoVersion string          `json:"cargoVersion"`
	CargoCksum   string          `json:"cargoCksum"`
	CargoSize    int64           `json:"cargoSize"`
	CargoBlobSHA string          `json:"cargoBlobSHA256"`
	CargoMeta    json.RawMessage `json:"cargoMeta"`
}

type CargoYank struct {
	Context      []string `json:"@context"`
	Type         string   `json:"type"`
	ID           string   `json:"id"`
	Published    string   `json:"published"`
	CargoName    string   `json:"cargoName"`
	CargoVersion string   `json:"cargoVersion"`
	CargoYanked  bool     `json:"cargoYanked"`
}

type federationAdapter struct {
	backend *Backend
}

func (b *Backend) FederationAdapter() activitypub.FederationAdapter {
	return &federationAdapter{backend: b}
}

func (a *federationAdapter) PackageType() string { return packageType }
func (a *federationAdapter) APTypes() []string   { return []string{apTypeVersion, apTypeYank} }

func (a *federationAdapter) Ingest(ctx context.Context, _, apType string, obj map[string]any, actorURL string) error {
	switch apType {
	case apTypeVersion:
		return a.ingestVersion(ctx, obj, actorURL)
	case apTypeYank:
		return a.applyYank(ctx, obj, actorURL)
	}
	return nil
}

func (a *federationAdapter) ingestVersion(ctx context.Context, obj map[string]any, actorURL string) error {
	name, _ := obj["cargoName"].(string)
	version, _ := obj["cargoVersion"].(string)
	if name == "" || version == "" {
		return fmt.Errorf("cargo version: missing cargoName or cargoVersion")
	}
	dbPkg, err := a.backend.db.GetOrCreatePackage(ctx, packageType, name, actorURL)
	if err != nil {
		return fmt.Errorf("cargo version: get-or-create: %w", err)
	}

	cksum, _ := obj["cargoCksum"].(string)
	size, _ := obj["cargoSize"].(float64)
	blobSHA, _ := obj["cargoBlobSHA256"].(string)

	stored := storedVersion{Cksum: cksum}
	if meta, ok := obj["cargoMeta"]; ok && meta != nil {
		if err := pkgfed.RemarshalInto(meta, &stored.Meta); err != nil {
			return fmt.Errorf("cargo version: decode meta: %w", err)
		}
	}
	metaBytes, err := json.Marshal(stored)
	if err != nil {
		return fmt.Errorf("cargo version: encode stored: %w", err)
	}

	v := &database.PackageVersion{
		PackageID:   dbPkg.ID,
		Version:     version,
		Metadata:    metaBytes,
		MediaType:   versionMediaType,
		SizeBytes:   int64(len(metaBytes)),
		SourceActor: &actorURL,
	}
	if err := a.backend.db.PutPackageVersion(ctx, v); err != nil {
		return fmt.Errorf("cargo version: put version: %w", err)
	}

	if blobSHA != "" {
		sizeBytes, err := pkgfed.ValidateBlobRef(blobSHA, size)
		if err != nil {
			return fmt.Errorf("cargo version: %w", err)
		}
		contentType := crateMediaType
		if err := a.backend.db.PutBlob(ctx, blobSHA, sizeBytes, &contentType, false); err != nil {
			return fmt.Errorf("cargo version: put blob ref: %w", err)
		}
		if err := pkgfed.RecordPeerBlob(ctx, a.backend.db, actorURL, blobSHA); err != nil {
			return fmt.Errorf("cargo version: put peer blob: %w", err)
		}
		if a.backend.replicator != nil {
			if peer := activitypub.EndpointFromActorURL(actorURL); peer != "" {
				a.backend.replicator.ReplicateFromURL(ctx, peerCrateURL(peer, name, version), blobSHA)
			}
		}
		file := &database.PackageFile{
			VersionID:   v.ID,
			Filename:    crateFilename(name, version),
			BlobDigest:  blobSHA,
			SizeBytes:   sizeBytes,
			ContentType: &contentType,
		}
		if err := a.backend.db.PutPackageFile(ctx, file); err != nil {
			return fmt.Errorf("cargo version: put file: %w", err)
		}
	}
	return nil
}

func (a *federationAdapter) applyYank(ctx context.Context, obj map[string]any, actorURL string) error {
	name, _ := obj["cargoName"].(string)
	version, _ := obj["cargoVersion"].(string)
	yanked, _ := obj["cargoYanked"].(bool)
	if name == "" || version == "" {
		return fmt.Errorf("cargo yank: missing cargoName or cargoVersion")
	}
	dbPkg, err := pkgfed.LookupOwnedPackage(ctx, a.backend.db, packageType, name, actorURL)
	if err != nil || dbPkg == nil {
		return err
	}
	v, err := a.backend.db.GetPackageVersion(ctx, dbPkg.ID, version)
	if err != nil || v == nil {
		return err
	}
	var stored storedVersion
	if err := json.Unmarshal(v.Metadata, &stored); err != nil {
		return fmt.Errorf("cargo yank: decode stored: %w", err)
	}
	stored.Yanked = yanked
	updated, err := json.Marshal(stored)
	if err != nil {
		return fmt.Errorf("cargo yank: encode stored: %w", err)
	}
	v.Metadata = updated
	v.SizeBytes = int64(len(updated))
	return a.backend.db.PutPackageVersion(ctx, v)
}

func (b *Backend) publishVersion(ctx context.Context, name string, v *database.PackageVersion, file *database.PackageFile, cksum string, meta any) {
	if b.publisher == nil {
		return
	}
	rawMeta, err := json.Marshal(meta)
	if err != nil {
		b.logger.Warn("cargo federation: marshal meta", "err", err)
		return
	}
	obj := CargoVersion{
		Context:      activitypub.BaseContext(),
		Type:         apTypeVersion,
		ID:           b.endpoint + "/ap/objects/cargo-version/" + url.PathEscape(name) + "/" + url.PathEscape(v.Version),
		Published:    activitypub.NowRFC3339(),
		CargoName:    name,
		CargoVersion: v.Version,
		CargoCksum:   cksum,
		CargoSize:    file.SizeBytes,
		CargoBlobSHA: file.BlobDigest,
		CargoMeta:    rawMeta,
	}
	if err := b.publisher.Publish(ctx, "Create", obj); err != nil {
		b.logger.Warn("cargo federation: publish version", "err", err, "name", name, "version", v.Version)
	}
}

func (b *Backend) publishYank(ctx context.Context, name, version string, yanked bool) {
	if b.publisher == nil {
		return
	}
	obj := CargoYank{
		Context:      activitypub.BaseContext(),
		Type:         apTypeYank,
		ID:           b.endpoint + "/ap/objects/cargo-yank/" + url.PathEscape(name) + "/" + url.PathEscape(version),
		Published:    activitypub.NowRFC3339(),
		CargoName:    name,
		CargoVersion: version,
		CargoYanked:  yanked,
	}
	if err := b.publisher.Publish(ctx, "Update", obj); err != nil {
		b.logger.Warn("cargo federation: publish yank", "err", err, "name", name, "version", version, "yanked", yanked)
	}
}

func (b *Backend) redirectToPeer(ctx context.Context, w http.ResponseWriter, r *http.Request, digest, name, version string) bool {
	peers, err := b.db.FindPeersWithBlob(ctx, digest)
	if err != nil || len(peers) == 0 {
		return false
	}
	http.Redirect(w, r, peerCrateURL(peers[0].PeerEndpoint, name, version), http.StatusFound) //nolint:gosec // peer endpoint sourced from authenticated federation activity
	return true
}

func peerCrateURL(peerEndpoint, name, version string) string {
	return peerEndpoint + routePrefix + "/api/v1/crates/" + url.PathEscape(name) + "/" + url.PathEscape(version) + "/download"
}

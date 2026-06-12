package nuget

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

const apTypePackage = "NugetPackage"

type NugetPackage struct {
	Context      []string `json:"@context"`
	Type         string   `json:"type"`
	ID           string   `json:"id"`
	Published    string   `json:"published"`
	NugetID      string   `json:"nugetId"`
	NugetVersion string   `json:"nugetVersion"`
	NugetFile    string   `json:"nugetFile"`
	NugetBlobSHA string   `json:"nugetBlobSHA256"`
	NugetSize    int64    `json:"nugetSize"`
	NugetURL     string   `json:"nugetURL"`
	NugetMeta    []byte   `json:"nugetMeta,omitempty"`
}

type federationAdapter struct {
	backend *Backend
}

func (b *Backend) FederationAdapter() activitypub.FederationAdapter {
	return &federationAdapter{backend: b}
}

func (a *federationAdapter) PackageType() string { return packageType }
func (a *federationAdapter) APTypes() []string   { return []string{apTypePackage} }

func (a *federationAdapter) Ingest(ctx context.Context, _, apType string, obj map[string]any, actorURL string) error {
	if apType != apTypePackage {
		return nil
	}
	return a.ingestPackage(ctx, obj, actorURL)
}

func (a *federationAdapter) ingestPackage(ctx context.Context, obj map[string]any, actorURL string) error {
	rawID, _ := obj["nugetId"].(string)
	version, _ := obj["nugetVersion"].(string)
	filename, _ := obj["nugetFile"].(string)
	if rawID == "" || version == "" || filename == "" {
		return fmt.Errorf("nuget package: missing id, version, or filename")
	}
	pkgID := normalizeID(rawID)

	dbPkg, err := a.backend.db.GetOrCreatePackage(ctx, packageType, pkgID, actorURL)
	if err != nil {
		return fmt.Errorf("nuget package: get-or-create: %w", err)
	}

	v, err := a.backend.db.GetPackageVersion(ctx, dbPkg.ID, version)
	if err != nil {
		return fmt.Errorf("nuget package: lookup version: %w", err)
	}
	if v == nil {
		stored := storedVersion{ID: rawID}
		if meta, ok := obj["nugetMeta"]; ok && meta != nil {
			if err := pkgfed.RemarshalInto(meta, &stored); err != nil {
				return fmt.Errorf("nuget package: decode meta: %w", err)
			}
		}
		raw, err := json.Marshal(stored)
		if err != nil {
			return fmt.Errorf("nuget package: encode stored: %w", err)
		}
		v = &database.PackageVersion{
			PackageID:   dbPkg.ID,
			Version:     version,
			Metadata:    raw,
			MediaType:   versionMediaType,
			SizeBytes:   int64(len(raw)),
			SourceActor: &actorURL,
		}
		if err := a.backend.db.PutPackageVersion(ctx, v); err != nil {
			return fmt.Errorf("nuget package: put version: %w", err)
		}
	}

	blobSHA, _ := obj["nugetBlobSHA256"].(string)
	size, _ := obj["nugetSize"].(float64)
	ct := nupkgMediaType
	if blobSHA != "" {
		if err := a.backend.db.PutBlob(ctx, blobSHA, int64(size), &ct, false); err != nil {
			return fmt.Errorf("nuget package: put blob ref: %w", err)
		}
		if err := pkgfed.RecordPeerBlob(ctx, a.backend.db, actorURL, blobSHA); err != nil {
			return fmt.Errorf("nuget package: put peer blob: %w", err)
		}
		if a.backend.replicator != nil {
			if peer := activitypub.EndpointFromActorURL(actorURL); peer != "" {
				a.backend.replicator.ReplicateFromURL(ctx, peerFileURL(peer, pkgID, version, filename), blobSHA)
			}
		}
	}

	file := &database.PackageFile{
		VersionID:   v.ID,
		Filename:    filename,
		BlobDigest:  blobSHA,
		SizeBytes:   int64(size),
		ContentType: &ct,
	}
	if err := a.backend.db.PutPackageFile(ctx, file); err != nil {
		return fmt.Errorf("nuget package: put file: %w", err)
	}
	return nil
}

func (b *Backend) publishPackage(ctx context.Context, pkgID, version string, file *database.PackageFile, stored storedVersion) {
	if b.publisher == nil {
		return
	}
	rawMeta, err := json.Marshal(stored)
	if err != nil {
		b.logger.Warn("nuget federation: marshal meta", "err", err)
		return
	}
	obj := NugetPackage{
		Context:      activitypub.BaseContext(),
		Type:         apTypePackage,
		ID:           b.endpoint + "/ap/objects/nuget-package/" + url.PathEscape(pkgID) + "/" + url.PathEscape(version),
		Published:    activitypub.NowRFC3339(),
		NugetID:      pkgID,
		NugetVersion: version,
		NugetFile:    file.Filename,
		NugetBlobSHA: file.BlobDigest,
		NugetSize:    file.SizeBytes,
		NugetURL:     b.packageURL(pkgID, version, file.Filename),
		NugetMeta:    rawMeta,
	}
	if err := b.publisher.Publish(ctx, "Create", obj); err != nil {
		b.logger.Warn("nuget federation: publish package", "err", err, "id", pkgID, "version", version)
	}
}

func (b *Backend) redirectToPeer(ctx context.Context, w http.ResponseWriter, r *http.Request, digest, pkgID, version, filename string) bool {
	peers, err := b.db.FindPeersWithBlob(ctx, digest)
	if err != nil || len(peers) == 0 {
		return false
	}
	http.Redirect(w, r, peerFileURL(peers[0].PeerEndpoint, pkgID, version, filename), http.StatusFound) //nolint:gosec // peer endpoint sourced from authenticated federation activity
	return true
}

func (b *Backend) packageURL(pkgID, version, filename string) string {
	return b.endpoint + routePrefix + "/v3-flatcontainer/" +
		url.PathEscape(pkgID) + "/" +
		url.PathEscape(version) + "/" +
		url.PathEscape(filename)
}

func peerFileURL(peerEndpoint, pkgID, version, filename string) string {
	return peerEndpoint + routePrefix + "/v3-flatcontainer/" +
		url.PathEscape(pkgID) + "/" +
		url.PathEscape(version) + "/" +
		url.PathEscape(filename)
}

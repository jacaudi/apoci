package pypi

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

const apTypeFile = "PypiFile"

// PypiFile: peers store metadata; file requests are 302'd to a peer that holds
// the blob.
type PypiFile struct {
	Context         []string        `json:"@context"`
	Type            string          `json:"type"`
	ID              string          `json:"id"`
	Published       string          `json:"published"`
	PypiName        string          `json:"pypiName"`
	PypiVersion     string          `json:"pypiVersion"`
	PypiFilename    string          `json:"pypiFilename"`
	PypiBlobSHA     string          `json:"pypiBlobSHA256"`
	PypiSize        int64           `json:"pypiSize"`
	PypiContentType string          `json:"pypiContentType"`
	PypiURL         string          `json:"pypiURL"`
	PypiMeta        json.RawMessage `json:"pypiMeta,omitempty"`
}

type federationAdapter struct {
	backend *Backend
}

func (b *Backend) FederationAdapter() activitypub.FederationAdapter {
	return &federationAdapter{backend: b}
}

func (a *federationAdapter) PackageType() string { return packageType }
func (a *federationAdapter) APTypes() []string   { return []string{apTypeFile} }

func (a *federationAdapter) Ingest(ctx context.Context, _, apType string, obj map[string]any, actorURL string) error {
	if apType != apTypeFile {
		return nil
	}
	return a.ingestFile(ctx, obj, actorURL)
}

func (a *federationAdapter) ingestFile(ctx context.Context, obj map[string]any, actorURL string) error {
	rawName, _ := obj["pypiName"].(string)
	version, _ := obj["pypiVersion"].(string)
	filename, _ := obj["pypiFilename"].(string)
	if rawName == "" || version == "" || filename == "" {
		return fmt.Errorf("pypi file: missing name, version, or filename")
	}
	name := normalizeName(rawName)
	dbPkg, err := a.backend.db.GetOrCreatePackage(ctx, packageType, name, actorURL)
	if err != nil {
		return fmt.Errorf("pypi file: get-or-create: %w", err)
	}

	v, err := a.backend.db.GetPackageVersion(ctx, dbPkg.ID, version)
	if err != nil {
		return fmt.Errorf("pypi file: lookup version: %w", err)
	}
	if v == nil {
		stored := storedVersion{}
		if meta, ok := obj["pypiMeta"]; ok && meta != nil {
			if err := pkgfed.RemarshalInto(meta, &stored.Meta); err != nil {
				return fmt.Errorf("pypi file: decode meta: %w", err)
			}
		}
		raw, err := json.Marshal(stored)
		if err != nil {
			return fmt.Errorf("pypi file: encode stored: %w", err)
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
			return fmt.Errorf("pypi file: put version: %w", err)
		}
	}

	blobSHA, _ := obj["pypiBlobSHA256"].(string)
	size, _ := obj["pypiSize"].(float64)
	contentType, _ := obj["pypiContentType"].(string)
	if contentType == "" {
		contentType = contentTypeForFilename(filename)
	}
	if blobSHA != "" {
		if err := a.backend.db.PutBlob(ctx, blobSHA, int64(size), &contentType, false); err != nil {
			return fmt.Errorf("pypi file: put blob ref: %w", err)
		}
		if err := pkgfed.RecordPeerBlob(ctx, a.backend.db, actorURL, blobSHA); err != nil {
			return fmt.Errorf("pypi file: put peer blob: %w", err)
		}
	}
	file := &database.PackageFile{
		VersionID:   v.ID,
		Filename:    filename,
		BlobDigest:  blobSHA,
		SizeBytes:   int64(size),
		ContentType: &contentType,
	}
	if err := a.backend.db.PutPackageFile(ctx, file); err != nil {
		return fmt.Errorf("pypi file: put file: %w", err)
	}
	return nil
}

func (b *Backend) publishFile(ctx context.Context, name, version string, file *database.PackageFile, meta any) {
	if b.publisher == nil {
		return
	}
	rawMeta, err := json.Marshal(meta)
	if err != nil {
		b.logger.Warn("pypi federation: marshal meta", "err", err)
		return
	}
	contentType := ""
	if file.ContentType != nil {
		contentType = *file.ContentType
	}
	obj := PypiFile{
		Context:         activitypub.BaseContext(),
		Type:            apTypeFile,
		ID:              b.endpoint + "/ap/objects/pypi-file/" + url.PathEscape(name) + "/" + url.PathEscape(version) + "/" + url.PathEscape(file.Filename),
		Published:       activitypub.NowRFC3339(),
		PypiName:        name,
		PypiVersion:     version,
		PypiFilename:    file.Filename,
		PypiBlobSHA:     file.BlobDigest,
		PypiSize:        file.SizeBytes,
		PypiContentType: contentType,
		PypiURL:         b.fileURL(name, version, file.Filename),
		PypiMeta:        rawMeta,
	}
	if err := b.publisher.Publish(ctx, "Create", obj); err != nil {
		b.logger.Warn("pypi federation: publish file", "err", err, "name", name, "version", version, "file", file.Filename)
	}
}

func (b *Backend) redirectToPeer(ctx context.Context, w http.ResponseWriter, r *http.Request, digest, name, version, filename string) bool {
	peers, err := b.db.FindPeersWithBlob(ctx, digest)
	if err != nil || len(peers) == 0 {
		return false
	}
	target := peers[0].PeerEndpoint + routePrefix + "/files/" + url.PathEscape(name) + "/" + url.PathEscape(version) + "/" + url.PathEscape(filename)
	http.Redirect(w, r, target, http.StatusFound)
	return true
}

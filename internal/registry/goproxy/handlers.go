package goproxy

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/blobstore"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
)

const (
	maxUploadBytes   = 500 << 20 // 500 MiB
	versionMediaType = "application/json"
)

// modInfo is the JSON body served at "<module>/@v/<version>.info" and "@latest".
type modInfo struct {
	Version string `json:"Version"`
	Time    string `json:"Time"`
}

func (b *Backend) handleList(w http.ResponseWriter, r *http.Request, mod string) {
	ctx := r.Context()
	if pkg, _ := b.db.GetPackage(ctx, packageType, mod); pkg != nil {
		versions, err := b.db.ListPackageVersions(ctx, pkg.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "list versions")
			return
		}
		if len(versions) > 0 {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			for _, v := range versions {
				esc, err := module.EscapeVersion(v.Version)
				if err != nil {
					continue
				}
				_, _ = io.WriteString(w, esc+"\n") //nolint:gosec // version strings, validated on ingest
			}
			return
		}
	}
	if b.upstreamEnabled() {
		if data, err := b.upstream.FetchList(ctx, mod); err == nil {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write(data) //nolint:gosec // proxied upstream module list (text/plain)
			return
		}
	}
	// No versions locally and no upstream hit: an empty list is a valid 200.
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
}

func (b *Backend) handleInfo(w http.ResponseWriter, r *http.Request, mod, ver string) {
	v := b.resolveVersion(r.Context(), mod, ver)
	if v == nil {
		writeError(w, http.StatusNotFound, "version not found")
		return
	}
	w.Header().Set("Content-Type", versionMediaType)
	if len(v.Metadata) > 0 {
		_, _ = w.Write(v.Metadata)
		return
	}
	writeJSON(w, modInfo{Version: ver, Time: v.CreatedAt.UTC().Format(time.RFC3339)})
}

func (b *Backend) handleMod(w http.ResponseWriter, r *http.Request, mod, ver string) {
	b.serveFile(w, r, mod, ver, ".mod", modMediaType)
}

func (b *Backend) handleZip(w http.ResponseWriter, r *http.Request, mod, ver string) {
	b.serveFile(w, r, mod, ver, ".zip", zipMediaType)
}

func (b *Backend) serveFile(w http.ResponseWriter, r *http.Request, mod, ver, suffix, contentType string) {
	ctx := r.Context()
	v := b.resolveVersion(ctx, mod, ver)
	if v == nil {
		writeError(w, http.StatusNotFound, "version not found")
		return
	}
	file, err := b.db.GetPackageFile(ctx, v.ID, ver+suffix)
	if err != nil || file == nil {
		writeError(w, http.StatusNotFound, "file not found")
		return
	}

	rc, size, err := b.blobs.Open(ctx, file.BlobDigest)
	if err != nil {
		if errors.Is(err, blobstore.ErrBlobNotFound) {
			if b.redirectToPeer(ctx, w, r, file.BlobDigest, mod, ver, suffix) {
				return
			}
			writeError(w, http.StatusNotFound, "blob missing")
			return
		}
		writeError(w, http.StatusInternalServerError, "open blob")
		return
	}
	defer func() { _ = rc.Close() }()

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	if _, err := io.Copy(w, rc); err != nil {
		b.logger.Warn("goproxy: copy failed", "err", err, "module", mod, "version", ver)
	}
}

func (b *Backend) handleLatest(w http.ResponseWriter, r *http.Request, mod string) {
	ctx := r.Context()
	if b.upstreamEnabled() {
		if data, err := b.upstream.FetchLatest(ctx, mod); err == nil {
			w.Header().Set("Content-Type", versionMediaType)
			_, _ = w.Write(data) //nolint:gosec // proxied upstream .info JSON
			return
		}
	}
	pkg, _ := b.db.GetPackage(ctx, packageType, mod)
	if pkg == nil {
		writeError(w, http.StatusNotFound, "module not found")
		return
	}
	versions, err := b.db.ListPackageVersions(ctx, pkg.ID)
	if err != nil || len(versions) == 0 {
		writeError(w, http.StatusNotFound, "no versions")
		return
	}
	latest := versions[0] // ListPackageVersions orders created_at DESC
	w.Header().Set("Content-Type", versionMediaType)
	if len(latest.Metadata) > 0 {
		_, _ = w.Write(latest.Metadata)
		return
	}
	writeJSON(w, modInfo{Version: latest.Version, Time: latest.CreatedAt.UTC().Format(time.RFC3339)})
}

func (b *Backend) handleUpload(w http.ResponseWriter, r *http.Request, mod, ver string) {
	ctx := r.Context()

	if err := module.Check(mod, ver); err != nil {
		writeError(w, http.StatusBadRequest, "invalid module/version: "+err.Error())
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "read upload: "+err.Error())
		return
	}

	goMod, err := extractGoMod(body, mod, ver)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid module zip: "+err.Error())
		return
	}

	dbPkg, err := b.db.GetOrCreatePackage(ctx, packageType, mod, b.owner)
	if err != nil {
		if errors.Is(err, database.ErrPackageOwnerMismatch) {
			writeError(w, http.StatusForbidden, "module is owned by another actor")
			return
		}
		writeError(w, http.StatusInternalServerError, "package access")
		return
	}

	existing, err := b.db.GetPackageVersion(ctx, dbPkg.ID, ver)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "lookup version")
		return
	}
	if existing != nil {
		writeError(w, http.StatusConflict, "version already exists")
		return
	}

	zipDigest, err := b.storeBlob(ctx, body, zipMediaType)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store zip")
		return
	}
	modDigest, err := b.storeBlob(ctx, goMod, modMediaType)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store go.mod")
		return
	}

	info := modInfo{Version: ver, Time: time.Now().UTC().Format(time.RFC3339)}
	infoRaw, _ := json.Marshal(info)
	v := &database.PackageVersion{
		PackageID: dbPkg.ID,
		Version:   ver,
		Metadata:  infoRaw,
		MediaType: versionMediaType,
		SizeBytes: int64(len(infoRaw)),
	}
	if err := b.db.PutPackageVersion(ctx, v); err != nil {
		writeError(w, http.StatusInternalServerError, "put version")
		return
	}

	zipCT, modCT := zipMediaType, modMediaType
	zipFile := &database.PackageFile{VersionID: v.ID, Filename: ver + ".zip", BlobDigest: zipDigest, SizeBytes: int64(len(body)), ContentType: &zipCT}
	modFile := &database.PackageFile{VersionID: v.ID, Filename: ver + ".mod", BlobDigest: modDigest, SizeBytes: int64(len(goMod)), ContentType: &modCT}
	for _, f := range []*database.PackageFile{zipFile, modFile} {
		if err := b.db.PutPackageFile(ctx, f); err != nil {
			writeError(w, http.StatusInternalServerError, "put file")
			return
		}
	}

	b.publishModule(ctx, mod, ver, zipFile, infoRaw)
	w.WriteHeader(http.StatusCreated)
}

// resolveVersion returns the version from local storage, falling back to
// caching it from an upstream proxy. Returns nil when neither has it.
func (b *Backend) resolveVersion(ctx context.Context, mod, ver string) *database.PackageVersion {
	if pkg, _ := b.db.GetPackage(ctx, packageType, mod); pkg != nil {
		if v, _ := b.db.GetPackageVersion(ctx, pkg.ID, ver); v != nil {
			return v
		}
	}
	if b.upstreamEnabled() {
		return b.cacheFromUpstream(ctx, mod, ver)
	}
	return nil
}

// cacheFromUpstream fetches a module version from an upstream proxy and persists
// it (owned by upstreamOwner) so subsequent requests are served locally.
func (b *Backend) cacheFromUpstream(ctx context.Context, mod, ver string) *database.PackageVersion {
	zipBytes, err := b.upstream.FetchZip(ctx, mod, ver)
	if err != nil {
		return nil
	}
	// Validate the upstream zip and pull its go.mod before persisting: clients
	// are told to disable GOSUMDB, so apoci is the only check against a bad upstream.
	goMod, err := extractGoMod(zipBytes, mod, ver)
	if err != nil {
		b.logger.Warn("goproxy cache: invalid upstream zip", "err", err, "module", mod, "version", ver)
		return nil
	}

	zipCT, modCT := zipMediaType, modMediaType
	zipDigest, err := b.storeBlob(ctx, zipBytes, zipCT)
	if err != nil {
		return nil
	}
	modDigest, err := b.storeBlob(ctx, goMod, modCT)
	if err != nil {
		return nil
	}

	infoRaw, err := b.upstream.FetchInfo(ctx, mod, ver)
	if err != nil || len(infoRaw) == 0 {
		infoRaw, _ = json.Marshal(modInfo{Version: ver, Time: time.Now().UTC().Format(time.RFC3339)})
	}

	pkg, err := b.db.GetOrCreatePackage(ctx, packageType, mod, upstreamOwner)
	if err != nil {
		b.logger.Warn("goproxy cache: package access", "err", err, "module", mod)
		return nil
	}
	v := &database.PackageVersion{
		PackageID: pkg.ID,
		Version:   ver,
		Metadata:  infoRaw,
		MediaType: versionMediaType,
		SizeBytes: int64(len(infoRaw)),
	}
	if err := b.db.PutPackageVersion(ctx, v); err != nil {
		return nil
	}
	zipFile := &database.PackageFile{VersionID: v.ID, Filename: ver + ".zip", BlobDigest: zipDigest, SizeBytes: int64(len(zipBytes)), ContentType: &zipCT}
	modFile := &database.PackageFile{VersionID: v.ID, Filename: ver + ".mod", BlobDigest: modDigest, SizeBytes: int64(len(goMod)), ContentType: &modCT}
	for _, f := range []*database.PackageFile{zipFile, modFile} {
		if err := b.db.PutPackageFile(ctx, f); err != nil {
			return nil
		}
	}
	b.logger.Info("goproxy: cached module from upstream", "module", mod, "version", ver, "size", len(zipBytes))
	return v
}

func (b *Backend) storeBlob(ctx context.Context, data []byte, mediaType string) (string, error) {
	digest, size, err := b.blobs.Put(ctx, bytes.NewReader(data), "")
	if err != nil {
		return "", err
	}
	if err := b.db.PutBlob(ctx, digest, size, &mediaType, true); err != nil {
		return "", err
	}
	return digest, nil
}

func (b *Backend) upstreamEnabled() bool {
	return b.upstream != nil && b.upstream.Enabled()
}

// extractGoMod validates the module zip's layout and returns its go.mod bytes.
// Module zips name every entry "<module>@<version>/...", so go.mod lives at
// "<module>@<version>/go.mod" and must declare the expected module path.
func extractGoMod(data []byte, mod, ver string) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	want := mod + "@" + ver + "/go.mod"
	for _, f := range zr.File {
		if f.Name != want {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		goMod, err := io.ReadAll(io.LimitReader(rc, 1<<20))
		_ = rc.Close()
		if err != nil {
			return nil, err
		}
		if declared := modfile.ModulePath(goMod); declared != mod {
			return nil, errors.New("go.mod declares module " + declared + ", expected " + mod)
		}
		return goMod, nil
	}
	return nil, errors.New("missing " + want)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", versionMediaType)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, msg+"\n") //nolint:gosec // text/plain, server-side message
}

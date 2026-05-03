package pypi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	gtpypi "code.gitea.io/gitea/modules/packages/pypi"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/blobstore"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
)

const (
	maxUploadBytes   = 200 << 20
	fileMediaType    = "application/octet-stream"
	versionMediaType = "application/json"
)

type storedVersion struct {
	Meta *gtpypi.Metadata `json:"meta"`
}

func (b *Backend) handleUpload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
		writePlainError(w, http.StatusBadRequest, "parse multipart: "+err.Error())
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	name := strings.TrimSpace(r.FormValue("name"))
	version := strings.TrimSpace(r.FormValue("version"))
	if name == "" || version == "" {
		writePlainError(w, http.StatusBadRequest, "name and version are required")
		return
	}
	normalizedName := normalizeName(name)

	files := r.MultipartForm.File["content"]
	if len(files) == 0 {
		writePlainError(w, http.StatusBadRequest, "content file is required")
		return
	}
	header := files[0]
	if header.Size > maxUploadBytes {
		writePlainError(w, http.StatusRequestEntityTooLarge, "upload too large")
		return
	}
	src, err := header.Open()
	if err != nil {
		writePlainError(w, http.StatusBadRequest, "open upload: "+err.Error())
		return
	}
	defer func() { _ = src.Close() }()

	body, err := io.ReadAll(io.LimitReader(src, maxUploadBytes+1))
	if err != nil {
		writePlainError(w, http.StatusBadRequest, "read upload: "+err.Error())
		return
	}
	if int64(len(body)) > maxUploadBytes {
		writePlainError(w, http.StatusRequestEntityTooLarge, "upload too large")
		return
	}

	if expected := r.FormValue("sha256_digest"); expected != "" {
		got := sha256.Sum256(body)
		if !strings.EqualFold(hex.EncodeToString(got[:]), expected) {
			writePlainError(w, http.StatusBadRequest, "sha256_digest mismatch")
			return
		}
	}

	digest, _, err := b.blobs.Put(ctx, bytes.NewReader(body), "")
	if err != nil {
		b.logger.Error("pypi upload: blob store failed", "err", err, "name", normalizedName, "version", version)
		writePlainError(w, http.StatusInternalServerError, "store blob")
		return
	}
	mediaType := fileMediaType
	if err := b.db.PutBlob(ctx, digest, int64(len(body)), &mediaType, true); err != nil {
		writePlainError(w, http.StatusInternalServerError, "record blob")
		return
	}

	dbPkg, err := b.db.GetOrCreatePackage(ctx, packageType, normalizedName, b.owner)
	if err != nil {
		if errors.Is(err, database.ErrPackageOwnerMismatch) {
			writePlainError(w, http.StatusForbidden, "package is owned by another actor")
			return
		}
		b.logger.Error("pypi upload: get-or-create package failed", "err", err, "name", normalizedName)
		writePlainError(w, http.StatusInternalServerError, "package access")
		return
	}

	v, err := b.db.GetPackageVersion(ctx, dbPkg.ID, version)
	if err != nil {
		writePlainError(w, http.StatusInternalServerError, "lookup version")
		return
	}
	if v == nil {
		stored := storedVersion{Meta: metadataFromForm(r)}
		raw, err := json.Marshal(stored)
		if err != nil {
			writePlainError(w, http.StatusInternalServerError, "encode metadata")
			return
		}
		v = &database.PackageVersion{
			PackageID: dbPkg.ID,
			Version:   version,
			Metadata:  raw,
			MediaType: versionMediaType,
			SizeBytes: int64(len(raw)),
		}
		if err := b.db.PutPackageVersion(ctx, v); err != nil {
			writePlainError(w, http.StatusInternalServerError, "put version")
			return
		}
	}

	if existing, err := b.db.GetPackageFile(ctx, v.ID, header.Filename); err != nil {
		writePlainError(w, http.StatusInternalServerError, "lookup file")
		return
	} else if existing != nil {
		writePlainError(w, http.StatusConflict, "file already exists: "+header.Filename)
		return
	}

	contentType := contentTypeForFilename(header.Filename)
	file := &database.PackageFile{
		VersionID:   v.ID,
		Filename:    header.Filename,
		BlobDigest:  digest,
		SizeBytes:   int64(len(body)),
		ContentType: &contentType,
	}
	if err := b.db.PutPackageFile(ctx, file); err != nil {
		writePlainError(w, http.StatusInternalServerError, "put file")
		return
	}

	b.publishFile(ctx, normalizedName, version, file, metadataFromForm(r))

	w.WriteHeader(http.StatusOK)
}

func metadataFromForm(r *http.Request) *gtpypi.Metadata {
	return &gtpypi.Metadata{
		Author:          r.FormValue("author"),
		Description:     r.FormValue("description"),
		LongDescription: r.FormValue("long_description"),
		Summary:         r.FormValue("summary"),
		ProjectURL:      r.FormValue("home_page"),
		License:         r.FormValue("license"),
		RequiresPython:  r.FormValue("requires_python"),
	}
}

func (b *Backend) handleSimpleRoot(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pkgs, err := b.db.ListPackages(ctx, packageType, "", 1<<30)
	if err != nil {
		writePlainError(w, http.StatusInternalServerError, "list packages")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, "<!DOCTYPE html><html><head><title>Simple Index</title></head><body>\n")
	for _, p := range pkgs {
		_, _ = fmt.Fprintf(w, "<a href=\"%s/\">%s</a><br>\n", html.EscapeString(p.Name), html.EscapeString(p.Name))
	}
	_, _ = io.WriteString(w, "</body></html>\n")
}

func (b *Backend) handleSimplePackage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := normalizeName(chi.URLParam(r, "name"))

	dbPkg, err := b.db.GetPackage(ctx, packageType, name)
	if err != nil {
		writePlainError(w, http.StatusInternalServerError, "lookup package")
		return
	}
	if dbPkg == nil {
		writePlainError(w, http.StatusNotFound, "project not found")
		return
	}

	versions, err := b.db.ListPackageVersions(ctx, dbPkg.ID)
	if err != nil {
		writePlainError(w, http.StatusInternalServerError, "list versions")
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w, "<!DOCTYPE html><html><head><title>Links for %s</title></head><body>\n<h1>Links for %s</h1>\n", html.EscapeString(name), html.EscapeString(name))

	for _, v := range versions {
		files, err := b.db.ListPackageFiles(ctx, v.ID)
		if err != nil {
			b.logger.Warn("pypi simple: list files failed", "err", err, "name", name, "version", v.Version)
			continue
		}
		var stored storedVersion
		_ = json.Unmarshal(v.Metadata, &stored)
		for _, f := range files {
			href := html.EscapeString(b.fileURL(name, v.Version, f.Filename) + "#sha256=" + cksumFromDigest(f.BlobDigest))
			extra := ""
			if stored.Meta != nil && stored.Meta.RequiresPython != "" {
				extra = ` data-requires-python="` + html.EscapeString(stored.Meta.RequiresPython) + `"`
			}
			_, _ = fmt.Fprintf(w, "<a href=\"%s\"%s>%s</a><br>\n", href, extra, html.EscapeString(f.Filename)) //nolint:gosec // all interpolated values are html.EscapeString'd
		}
	}

	_, _ = io.WriteString(w, "</body></html>\n")
}

func (b *Backend) handleDownload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := normalizeName(chi.URLParam(r, "name"))
	version := chi.URLParam(r, "version")
	filename := chi.URLParam(r, "filename")

	dbPkg, err := b.db.GetPackage(ctx, packageType, name)
	if err != nil {
		writePlainError(w, http.StatusInternalServerError, "lookup package")
		return
	}
	if dbPkg == nil {
		writePlainError(w, http.StatusNotFound, "project not found")
		return
	}
	v, err := b.db.GetPackageVersion(ctx, dbPkg.ID, version)
	if err != nil {
		writePlainError(w, http.StatusInternalServerError, "lookup version")
		return
	}
	if v == nil {
		writePlainError(w, http.StatusNotFound, "version not found")
		return
	}
	file, err := b.db.GetPackageFile(ctx, v.ID, filename)
	if err != nil || file == nil {
		writePlainError(w, http.StatusNotFound, "file not found")
		return
	}

	rc, size, err := b.blobs.Open(ctx, file.BlobDigest)
	if err != nil {
		if errors.Is(err, blobstore.ErrBlobNotFound) {
			if b.redirectToPeer(ctx, w, r, file.BlobDigest, name, version, filename) {
				return
			}
			writePlainError(w, http.StatusNotFound, "blob missing")
			return
		}
		writePlainError(w, http.StatusInternalServerError, "open blob")
		return
	}
	defer func() { _ = rc.Close() }()

	contentType := fileMediaType
	if file.ContentType != nil {
		contentType = *file.ContentType
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("ETag", `"`+file.BlobDigest+`"`)
	if _, err := io.Copy(w, rc); err != nil {
		b.logger.Warn("pypi download: copy failed", "err", err, "project", name, "version", version, "file", filename)
	}
}

func (b *Backend) fileURL(name, version, filename string) string {
	return b.endpoint + routePrefix + "/files/" +
		url.PathEscape(name) + "/" +
		url.PathEscape(version) + "/" +
		url.PathEscape(filename)
}

func contentTypeForFilename(filename string) string {
	switch {
	case strings.HasSuffix(filename, ".whl"):
		return "application/octet-stream"
	case strings.HasSuffix(filename, ".tar.gz"), strings.HasSuffix(filename, ".tgz"):
		return "application/x-gtar"
	case strings.HasSuffix(filename, ".zip"):
		return "application/zip"
	default:
		return "application/octet-stream"
	}
}

func cksumFromDigest(digest string) string {
	return strings.TrimPrefix(digest, "sha256:")
}

func writePlainError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, msg+"\n") //nolint:gosec // text/plain response, msg is from server-side strings
}

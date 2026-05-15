package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

func (db *DB) GetBlob(ctx context.Context, digest string) (*Blob, error) {
	b := &Blob{}
	err := db.bun.NewSelect().Model(b).Where("digest = ?", digest).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("querying blob: %w", err)
	}
	return b, nil
}

func (db *DB) PutBlob(ctx context.Context, digest string, sizeBytes int64, mediaType *string, storedLocally bool) error {
	_, err := db.bun.NewRaw(
		`INSERT INTO blobs (digest, size_bytes, media_type, stored_locally)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(digest) DO UPDATE SET
		   size_bytes = CASE WHEN excluded.stored_locally THEN excluded.size_bytes ELSE blobs.size_bytes END,
		   media_type = COALESCE(excluded.media_type, blobs.media_type),
		   stored_locally = blobs.stored_locally OR excluded.stored_locally`,
		digest, sizeBytes, mediaType, storedLocally).Exec(ctx)
	if err != nil {
		return fmt.Errorf("putting blob: %w", err)
	}
	return nil
}

// JOIN blobs disambiguates: package_files also records index→child manifest
// digests, which must not be served as blobs.
func (db *DB) BlobExistsInRepo(ctx context.Context, repoName string, digest string) (bool, error) {
	var exists bool
	err := db.bun.NewRaw(
		`SELECT EXISTS(
			SELECT 1 FROM package_files pf
			JOIN package_versions pv ON pv.id = pf.version_id
			JOIN packages p ON p.id = pv.package_id
			JOIN blobs b ON b.digest = pf.blob_digest
			WHERE p.type = 'oci' AND p.name = ? AND pf.blob_digest = ?
		)`, repoName, digest).Scan(ctx, &exists)
	if err != nil {
		return false, fmt.Errorf("checking blob in repo: %w", err)
	}
	return exists, nil
}

func (db *DB) FindRepoForBlob(ctx context.Context, digest string) (string, error) {
	var name string
	err := db.bun.NewRaw(
		`SELECT p.name FROM packages p
		 JOIN package_versions pv ON pv.package_id = p.id
		 JOIN package_files pf ON pf.version_id = pv.id
		 JOIN blobs b ON b.digest = pf.blob_digest
		 WHERE p.type = 'oci' AND pf.blob_digest = ?
		 LIMIT 1`, digest).Scan(ctx, &name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("finding repo for blob: %w", err)
	}
	return name, nil
}

func (db *DB) DeleteBlob(ctx context.Context, digest string) error {
	_, err := db.bun.NewRaw("DELETE FROM blobs WHERE digest = ?", digest).Exec(ctx)
	if err != nil {
		return fmt.Errorf("deleting blob: %w", err)
	}
	return nil
}

// OrphanedBlobs returns digests of blobs with no manifest or peer reference.
// A non-zero createdBefore protects in-flight uploads (PutBlob committed,
// manifest commit not yet) by excluding rows newer than the cutoff.
func (db *DB) OrphanedBlobs(ctx context.Context, limit int, createdBefore time.Time) ([]string, error) {
	var digests []string
	q := `SELECT b.digest FROM blobs b
	      WHERE NOT EXISTS (SELECT 1 FROM package_files pf WHERE pf.blob_digest = b.digest)
	        AND NOT EXISTS (SELECT 1 FROM peer_blobs pb WHERE pb.blob_digest = b.digest)`
	args := []any{}
	if !createdBefore.IsZero() {
		q += " AND b.created_at < ?"
		args = append(args, createdBefore)
	}
	q += " LIMIT ?"
	args = append(args, limit)
	if err := db.bun.NewRaw(q, args...).Scan(ctx, &digests); err != nil {
		return nil, fmt.Errorf("finding orphaned blobs: %w", err)
	}
	return digests, nil
}

// AllBlobDigests returns all blob digests known in the database, paging in batches of pageSize.
func (db *DB) AllBlobDigests(ctx context.Context, pageSize int) (map[string]bool, error) {
	digests := make(map[string]bool)
	var afterDigest string
	for {
		var batch []string
		err := db.bun.NewRaw(
			"SELECT digest FROM blobs WHERE digest > ? ORDER BY digest LIMIT ?",
			afterDigest, pageSize).Scan(ctx, &batch)
		if err != nil {
			return nil, fmt.Errorf("listing blob digests: %w", err)
		}
		for _, d := range batch {
			digests[d] = true
		}
		if len(batch) < pageSize {
			break
		}
		afterDigest = batch[len(batch)-1]
	}
	return digests, nil
}

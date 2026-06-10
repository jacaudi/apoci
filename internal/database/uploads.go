package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

func (db *DB) CreateUploadSession(ctx context.Context, uuid string, repoID int64, ttl time.Duration) (*UploadSession, error) {
	s := &UploadSession{
		UUID:         uuid,
		RepositoryID: repoID,
		ExpiresAt:    time.Now().Add(ttl),
		CreatedAt:    time.Now(),
	}
	_, err := db.bun.NewInsert().Model(s).Returning("id").Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("creating upload session: %w", err)
	}
	return s, nil
}

func (db *DB) GetUploadSession(ctx context.Context, uuid string) (*UploadSession, error) {
	s := &UploadSession{}
	// expires_at is written from time.Now(); compare against the same clock, not
	// the DB's CURRENT_TIMESTAMP, whose timezone/format differs by backend.
	err := db.bun.NewSelect().Model(s).
		Where("uuid = ?", uuid).
		Where("expires_at > ?", time.Now()).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("querying upload session: %w", err)
	}
	return s, nil
}

func (db *DB) DeleteUploadSession(ctx context.Context, uuid string) error {
	_, err := db.bun.NewRaw(
		"DELETE FROM upload_sessions WHERE uuid = ?", uuid).Exec(ctx)
	if err != nil {
		return fmt.Errorf("deleting upload session: %w", err)
	}
	return nil
}

// ListExpiredUploadSessions returns UUIDs of upload sessions that have passed their expiry time.
func (db *DB) ListExpiredUploadSessions(ctx context.Context, limit int) ([]string, error) {
	var uuids []string
	// App clock, not CURRENT_TIMESTAMP — see GetUploadSession.
	err := db.bun.NewRaw(
		"SELECT uuid FROM upload_sessions WHERE expires_at <= ? LIMIT ?",
		time.Now(), limit).Scan(ctx, &uuids)
	if err != nil {
		return nil, fmt.Errorf("listing expired upload sessions: %w", err)
	}
	return uuids, nil
}

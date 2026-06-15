package database

import (
	"context"
	"fmt"
	"time"
)

// EnqueueDelivery adds an activity delivery to the persistent queue.
func (db *DB) EnqueueDelivery(ctx context.Context, activityID, inboxURL string, activityJSON []byte) error {
	_, err := db.EnqueueDeliveryInserted(ctx, activityID, inboxURL, activityJSON)
	return err
}

// EnqueueDeliveryInserted enqueues a delivery and reports whether a new row was
// actually inserted (false means it was already queued — a no-op), so callers
// don't count idempotent re-enqueues in metrics or trigger spurious wakeups.
func (db *DB) EnqueueDeliveryInserted(ctx context.Context, activityID, inboxURL string, activityJSON []byte) (bool, error) {
	res, err := db.bun.NewRaw(
		`INSERT INTO delivery_queue (activity_id, inbox_url, activity_json)
		 VALUES (?, ?, ?)
		 ON CONFLICT (activity_id, inbox_url) DO NOTHING`,
		activityID, inboxURL, activityJSON).Exec(ctx)
	if err != nil {
		return false, fmt.Errorf("enqueuing delivery: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("checking delivery insert: %w", err)
	}
	return n > 0, nil
}

// PendingDeliveries returns deliveries ready to be attempted, up to limit.
func (db *DB) PendingDeliveries(ctx context.Context, limit int) ([]Delivery, error) {
	var deliveries []Delivery
	err := db.bun.NewSelect().Model(&deliveries).
		Where("status = 'pending'").
		Where("next_attempt_at <= CURRENT_TIMESTAMP").
		OrderExpr("next_attempt_at ASC").
		Limit(limit).
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("querying pending deliveries: %w", err)
	}
	return deliveries, nil
}

// MarkDelivered marks a delivery as successfully completed.
func (db *DB) MarkDelivered(ctx context.Context, id int64) error {
	_, err := db.bun.NewRaw(
		`UPDATE delivery_queue SET status = 'delivered', attempts = attempts + 1
		 WHERE id = ?`, id).Exec(ctx)
	if err != nil {
		return fmt.Errorf("marking delivery delivered: %w", err)
	}
	return nil
}

// MarkDeliveryFailed records a failed attempt and schedules retry with backoff.
func (db *DB) MarkDeliveryFailed(ctx context.Context, id int64, attempts, maxAttempts int, lastError string) error {
	backoff := time.Duration(1<<min(attempts, 12)) * time.Second
	nextAttempt := time.Now().Add(backoff)
	status := "pending"
	if attempts+1 >= maxAttempts {
		status = "failed"
	}
	_, err := db.bun.NewRaw(
		`UPDATE delivery_queue SET
		   attempts = attempts + 1,
		   last_error = ?,
		   status = ?,
		   next_attempt_at = ?
		 WHERE id = ?`, lastError, status, nextAttempt, id).Exec(ctx)
	if err != nil {
		return fmt.Errorf("marking delivery failed: %w", err)
	}
	return nil
}

// CleanupDeliveries removes completed/failed deliveries older than the given age.
func (db *DB) CleanupDeliveries(ctx context.Context, olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan)
	res, err := db.bun.NewRaw(
		`DELETE FROM delivery_queue
		 WHERE status IN ('delivered', 'failed') AND created_at < ?`, cutoff).Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("cleaning up deliveries: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

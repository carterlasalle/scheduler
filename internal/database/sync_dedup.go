package database

import (
	"context"
	"database/sql"
	"fmt"
)

// LoadSyncedContentHashes returns the full mem_key -> content_hash map, so
// callers can check membership in-memory without a DB round trip per key
// (avoids holding the single SQLite connection open mid row-iteration).
func LoadSyncedContentHashes(ctx context.Context, db *sql.DB) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT mem_key, content_hash FROM duckbrain_sync_dedup`)
	if err != nil {
		return nil, fmt.Errorf("query duckbrain_sync_dedup: %w", err)
	}
	defer rows.Close()

	out := make(map[string]string)
	for rows.Next() {
		var key, hash string
		if err := rows.Scan(&key, &hash); err != nil {
			return nil, fmt.Errorf("scan synced hash row: %w", err)
		}
		out[key] = hash
	}
	return out, rows.Err()
}

// RecordSyncedContentHash upserts the content hash for memKey after a
// successful DuckBrain post, so the next cycle can skip it if unchanged.
func RecordSyncedContentHash(ctx context.Context, db *sql.DB, memKey, hash string) error {
	_, err := db.ExecContext(ctx, `
INSERT INTO duckbrain_sync_dedup (mem_key, content_hash, synced_at)
VALUES (?, ?, ?)
ON CONFLICT(mem_key) DO UPDATE SET content_hash = excluded.content_hash, synced_at = excluded.synced_at
`, memKey, hash, nowUTC())
	if err != nil {
		return fmt.Errorf("record synced hash %q: %w", memKey, err)
	}
	return nil
}

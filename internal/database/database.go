package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/dialect/sqlitedialect"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/mattn/go-sqlite3"
)

type DB struct {
	bun    *bun.DB
	logger *slog.Logger
}

// OpenSQLite opens a SQLite database in the given data directory.
func OpenSQLite(dataDir string, maxOpen, maxIdle int, logger *slog.Logger) (*DB, error) {
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, fmt.Errorf("creating data directory: %w", err)
	}

	dbPath := filepath.Join(dataDir, "apoci.db")
	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_foreign_keys=ON&_busy_timeout=5000&_synchronous=NORMAL", dbPath)

	sqldb, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening sqlite database: %w", err)
	}

	if maxOpen <= 0 {
		maxOpen = 4
	}
	if maxIdle <= 0 {
		maxIdle = maxOpen
	}
	sqldb.SetMaxOpenConns(maxOpen)
	sqldb.SetMaxIdleConns(maxIdle)

	if err := sqldb.Ping(); err != nil {
		_ = sqldb.Close()
		return nil, fmt.Errorf("pinging database: %w", err)
	}

	bunDB := bun.NewDB(sqldb, sqlitedialect.New())

	db := &DB{bun: bunDB, logger: logger}
	if err := db.migrate(context.Background()); err != nil {
		_ = bunDB.Close()
		return nil, fmt.Errorf("running migrations: %w", err)
	}

	logger.Info("database opened", "path", dbPath)
	return db, nil
}

// OpenPostgres opens a PostgreSQL database with the given DSN.
func OpenPostgres(dsn string, maxOpen, maxIdle int, logger *slog.Logger) (*DB, error) {
	sqldb, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening postgres database: %w", err)
	}

	if maxOpen <= 0 {
		maxOpen = 25
	}
	if maxIdle <= 0 {
		maxIdle = 10
	}
	sqldb.SetMaxOpenConns(maxOpen)
	sqldb.SetMaxIdleConns(maxIdle)

	if err := sqldb.Ping(); err != nil {
		_ = sqldb.Close()
		return nil, fmt.Errorf("pinging database: %w", err)
	}

	bunDB := bun.NewDB(sqldb, pgdialect.New())

	db := &DB{bun: bunDB, logger: logger}
	if err := db.migrate(context.Background()); err != nil {
		_ = bunDB.Close()
		return nil, fmt.Errorf("running migrations: %w", err)
	}

	logger.Info("database opened", "driver", "postgres")
	return db, nil
}

func (db *DB) Ping() error {
	return db.bun.Ping()
}

// QueryContext exposes raw SQL — used by tests for assertions.
func (db *DB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return db.bun.QueryContext(ctx, query, args...)
}

// ExecContext exposes raw SQL — used by tests for setup.
func (db *DB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return db.bun.ExecContext(ctx, query, args...)
}

func (db *DB) Close() error {
	return db.bun.Close()
}

func (db *DB) tableExists(ctx context.Context, tableName string) (bool, error) {
	var n int
	query := "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?"
	if _, isPostgres := db.bun.Dialect().(*pgdialect.Dialect); isPostgres {
		query = "SELECT COUNT(*) FROM information_schema.tables WHERE table_name = ?"
	}
	if err := db.bun.NewRaw(query, tableName).Scan(ctx, &n); err != nil {
		return false, fmt.Errorf("checking for table %q: %w", tableName, err)
	}
	return n > 0, nil
}

func (db *DB) migrate(ctx context.Context) error {
	if _, err := db.bun.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("creating schema_version table: %w", err)
	}

	version := 0
	row := db.bun.QueryRowContext(ctx, `SELECT version FROM schema_version LIMIT 1`)
	if err := row.Scan(&version); err != nil {
		// Only an empty table means "not initialised". Any other error must abort,
		// else we'd treat it as version 0 and re-run every migration (incl. DROPs).
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("reading schema version: %w", err)
		}
		if _, err := db.bun.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (0)`); err != nil {
			return fmt.Errorf("initializing schema version: %w", err)
		}
	}

	steps := []func(context.Context) error{
		db.migrateV1, db.migrateV2, db.migrateV3, db.migrateV4, db.migrateV5,
		db.migrateV6, db.migrateV7, db.migrateV8, db.migrateV9,
	}
	for i, step := range steps {
		v := i + 1
		if version >= v {
			continue
		}
		if err := step(ctx); err != nil {
			return fmt.Errorf("migration v%d: %w", v, err)
		}
		if _, err := db.bun.ExecContext(ctx, `UPDATE schema_version SET version = ?`, v); err != nil {
			return fmt.Errorf("updating schema version to %d: %w", v, err)
		}
		version = v
	}

	return nil
}

// migrateV7 adds federation_tag_globs to actors.
// Idempotent: skips if the column already exists.
func (db *DB) migrateV7(ctx context.Context) error {
	return db.addColumnIfMissing(ctx, "actors", "federation_tag_globs", "TEXT")
}

func (db *DB) migrateV8(ctx context.Context) error {
	return db.addColumnIfMissing(ctx, "packages", "federation_withdrawn_at", "TIMESTAMP")
}

// migrateV9 drops the now-unused immutable column from package_tags.
func (db *DB) migrateV9(ctx context.Context) error {
	return db.dropColumnIfExists(ctx, "package_tags", "immutable")
}

func (db *DB) addColumnIfMissing(ctx context.Context, table, column, definition string) error {
	exists, err := db.columnExists(ctx, table, column)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	_, err = db.bun.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition))
	return err
}

func (db *DB) dropColumnIfExists(ctx context.Context, table, column string) error {
	exists, err := db.columnExists(ctx, table, column)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	_, err = db.bun.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s DROP COLUMN %s", table, column))
	return err
}

func (db *DB) columnExists(ctx context.Context, table, column string) (bool, error) {
	if _, isPostgres := db.bun.Dialect().(*pgdialect.Dialect); isPostgres {
		var n int
		err := db.bun.NewRaw(
			"SELECT COUNT(*) FROM information_schema.columns WHERE table_name = ? AND column_name = ?",
			table, column,
		).Scan(ctx, &n)
		if err != nil {
			return false, err
		}
		return n > 0, nil
	}
	// SQLite
	rows, err := db.bun.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notnull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, rows.Err()
		}
	}
	return false, rows.Err()
}

// migrateV6 unifies the OCI tables into the generic package schema. Row IDs
// are preserved so OCI callers that round-trip IDs stay consistent.
func (db *DB) migrateV6(ctx context.Context) error {
	models := []any{
		(*Package)(nil),
		(*PackageVersion)(nil),
		(*PackageFile)(nil),
		(*PackageTag)(nil),
		(*DeletedVersion)(nil),
	}
	for _, model := range models {
		if _, err := db.bun.NewCreateTable().Model(model).IfNotExists().Exec(ctx); err != nil {
			return fmt.Errorf("creating package table for %T: %w", model, err)
		}
	}

	indexes := []string{
		"CREATE UNIQUE INDEX IF NOT EXISTS idx_packages_type_name ON packages (type, name)",
		"CREATE INDEX IF NOT EXISTS idx_packages_owner ON packages (owner_id)",
		"CREATE UNIQUE INDEX IF NOT EXISTS idx_package_versions_pkg_version ON package_versions (package_id, version)",
		"CREATE INDEX IF NOT EXISTS idx_package_versions_digest ON package_versions (version)",
		"CREATE INDEX IF NOT EXISTS idx_package_versions_source_actor ON package_versions (source_actor)",
		"CREATE INDEX IF NOT EXISTS idx_package_versions_subject ON package_versions (package_id, subject_digest)",
		"CREATE UNIQUE INDEX IF NOT EXISTS idx_package_files_version_filename ON package_files (version_id, filename)",
		"CREATE INDEX IF NOT EXISTS idx_package_files_blob_digest ON package_files (blob_digest)",
		"CREATE UNIQUE INDEX IF NOT EXISTS idx_package_tags_pkg_name ON package_tags (package_id, name)",
		"CREATE INDEX IF NOT EXISTS idx_package_tags_version ON package_tags (version)",
		"CREATE INDEX IF NOT EXISTS idx_deleted_versions_lookup ON deleted_versions (package_type, version)",
		"CREATE UNIQUE INDEX IF NOT EXISTS idx_deleted_versions_unique ON deleted_versions (package_type, package_name, version)",
		"CREATE INDEX IF NOT EXISTS idx_deleted_versions_at ON deleted_versions (deleted_at)",
	}
	for _, ddl := range indexes {
		if _, err := db.bun.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("creating package index: %w", err)
		}
	}

	if err := db.copyV5DataToV6(ctx); err != nil {
		return fmt.Errorf("copying v5 data: %w", err)
	}

	if err := db.dropV5OCITables(ctx); err != nil {
		return fmt.Errorf("dropping legacy OCI tables: %w", err)
	}

	if _, isPostgres := db.bun.Dialect().(*pgdialect.Dialect); isPostgres {
		if err := db.resetV6PostgresSequences(ctx); err != nil {
			return fmt.Errorf("resetting postgres sequences: %w", err)
		}
	}

	return nil
}

func (db *DB) copyV5DataToV6(ctx context.Context) error {
	// SQLite's INSERT...SELECT rejects ON CONFLICT, so each guard is a
	// WHERE NOT EXISTS keyed on the table's natural unique constraint (not
	// id), and layers LEFT JOIN blobs so orphan rows are preserved.
	statements := []struct{ name, sql string }{
		{
			"packages from repositories",
			`INSERT INTO packages (id, type, name, owner_id, private, created_at)
			 SELECT r.id, 'oci', r.name, r.owner_id, r.private, r.created_at FROM repositories r
			 WHERE NOT EXISTS (SELECT 1 FROM packages p WHERE p.type = 'oci' AND p.name = r.name)`,
		},
		{
			"package_versions from manifests",
			`INSERT INTO package_versions (id, package_id, version, metadata, media_type, size_bytes, source_actor, subject_digest, artifact_type, created_at)
			 SELECT m.id, m.repository_id, m.digest, m.content, COALESCE(m.media_type, ''), m.size_bytes, m.source_actor, m.subject_digest, m.artifact_type, m.created_at
			 FROM manifests m
			 WHERE NOT EXISTS (SELECT 1 FROM package_versions pv WHERE pv.package_id = m.repository_id AND pv.version = m.digest)`,
		},
		{
			"package_files from manifest_layers",
			`INSERT INTO package_files (version_id, filename, blob_digest, size_bytes, content_type)
			 SELECT ml.manifest_id, ml.blob_digest, ml.blob_digest, COALESCE(b.size_bytes, 0), b.media_type
			 FROM manifest_layers ml
			 LEFT JOIN blobs b ON b.digest = ml.blob_digest
			 WHERE NOT EXISTS (
			   SELECT 1 FROM package_files pf
			   WHERE pf.version_id = ml.manifest_id AND pf.filename = ml.blob_digest
			 )`,
		},
		{
			"package_tags from tags",
			`INSERT INTO package_tags (id, package_id, name, version, updated_at)
			 SELECT t.id, t.repository_id, t.name, t.manifest_digest, t.updated_at FROM tags t
			 WHERE NOT EXISTS (SELECT 1 FROM package_tags pt WHERE pt.package_id = t.repository_id AND pt.name = t.name)`,
		},
		{
			"deleted_versions from deleted_manifests",
			`INSERT INTO deleted_versions (package_type, package_name, version, source_actor, deleted_at)
			 SELECT 'oci', dm.repo_name, dm.digest, dm.source_actor, dm.deleted_at FROM deleted_manifests dm
			 WHERE NOT EXISTS (
			   SELECT 1 FROM deleted_versions dv
			   WHERE dv.package_type = 'oci' AND dv.package_name = dm.repo_name AND dv.version = dm.digest
			 )`,
		},
	}
	for _, s := range statements {
		if _, err := db.bun.ExecContext(ctx, s.sql); err != nil {
			if isMissingTableErr(err) {
				continue
			}
			return fmt.Errorf("copying %s: %w", s.name, err)
		}
	}
	return nil
}

func (db *DB) dropV5OCITables(ctx context.Context) error {
	stmts := []string{
		"DROP INDEX IF EXISTS idx_manifests_repo_digest",
		"DROP INDEX IF EXISTS idx_tags_repo_name",
		"DROP INDEX IF EXISTS idx_repo_owners_pk",
		"DROP INDEX IF EXISTS idx_manifest_layers_pk",
		"DROP INDEX IF EXISTS idx_manifests_digest",
		"DROP INDEX IF EXISTS idx_manifests_repo",
		"DROP INDEX IF EXISTS idx_repositories_owner",
		"DROP INDEX IF EXISTS idx_tags_manifest_digest",
		"DROP INDEX IF EXISTS idx_manifest_layers_blob_digest",
		"DROP INDEX IF EXISTS idx_deleted_manifests_deleted_at",
		"DROP TABLE IF EXISTS manifest_layers",
		"DROP TABLE IF EXISTS tags",
		"DROP TABLE IF EXISTS manifests",
		"DROP TABLE IF EXISTS repository_owners",
		"DROP TABLE IF EXISTS repositories",
		"DROP TABLE IF EXISTS deleted_manifests",
	}
	for _, ddl := range stmts {
		if _, err := db.bun.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("executing %q: %w", ddl, err)
		}
	}
	return nil
}

// resetV6PostgresSequences advances auto-increment sequences past preserved
// IDs (Postgres doesn't update them on explicit-id inserts; SQLite does).
func (db *DB) resetV6PostgresSequences(ctx context.Context) error {
	tables := []string{"packages", "package_versions", "package_files", "package_tags", "deleted_versions"}
	for _, t := range tables {
		var maxID int64
		if err := db.bun.NewRaw(fmt.Sprintf("SELECT COALESCE(MAX(id), 0) FROM %s", t)).Scan(ctx, &maxID); err != nil {
			return fmt.Errorf("reading max id from %s: %w", t, err)
		}
		if maxID == 0 {
			continue
		}
		stmt := fmt.Sprintf(`SELECT setval(pg_get_serial_sequence('%s', 'id'), ?)`, t)
		if _, err := db.bun.ExecContext(ctx, stmt, maxID); err != nil {
			return fmt.Errorf("setval on %s: %w", t, err)
		}
	}
	return nil
}

func isMissingTableErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "no such table") ||
		strings.Contains(msg, "does not exist") ||
		strings.Contains(msg, "undefined table")
}

func (db *DB) migrateV4(ctx context.Context) error {
	exists, err := db.tableExists(ctx, "repositories")
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	return db.addColumnIfMissing(ctx, "repositories", "private", "BOOLEAN NOT NULL DEFAULT FALSE")
}

// migrateV1 creates the legacy OCI tables (dropped in v6) and the shared
// infrastructure tables. On a fresh install the OCI tables are created and
// immediately dropped by v6.
func (db *DB) migrateV1(ctx context.Context) error {
	bunModels := []any{
		(*Blob)(nil),
		(*PeerBlob)(nil),
		(*Actor)(nil),
		(*FollowRequest)(nil),
		(*Activity)(nil),
		(*UploadSession)(nil),
		(*Delivery)(nil),
	}
	for _, model := range bunModels {
		if _, err := db.bun.NewCreateTable().Model(model).IfNotExists().Exec(ctx); err != nil {
			return fmt.Errorf("creating table for %T: %w", model, err)
		}
	}

	_, isPostgres := db.bun.Dialect().(*pgdialect.Dialect)
	autoIncID := "INTEGER PRIMARY KEY AUTOINCREMENT"
	if isPostgres {
		autoIncID = "BIGSERIAL PRIMARY KEY"
	}

	legacyTables := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS repositories (
			id %s,
			name TEXT NOT NULL UNIQUE,
			owner_id TEXT NOT NULL,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`, autoIncID),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS manifests (
			id %s,
			repository_id BIGINT NOT NULL,
			digest TEXT NOT NULL,
			media_type TEXT NOT NULL,
			size_bytes BIGINT NOT NULL,
			content BYTEA,
			source_actor TEXT,
			subject_digest TEXT,
			artifact_type TEXT,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`, autoIncID),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS tags (
			id %s,
			repository_id BIGINT NOT NULL,
			name TEXT NOT NULL,
			manifest_digest TEXT NOT NULL,
			immutable BOOLEAN NOT NULL DEFAULT FALSE,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`, autoIncID),
		`CREATE TABLE IF NOT EXISTS repository_owners (
			repository_id BIGINT NOT NULL,
			owner_id TEXT NOT NULL,
			granted_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS manifest_layers (
			manifest_id BIGINT NOT NULL,
			blob_digest TEXT NOT NULL
		)`,
	}
	if !isPostgres {
		for i := range legacyTables {
			legacyTables[i] = strings.ReplaceAll(legacyTables[i], "BYTEA", "BLOB")
			legacyTables[i] = strings.ReplaceAll(legacyTables[i], "BIGINT", "INTEGER")
		}
	}
	for _, ddl := range legacyTables {
		if _, err := db.bun.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("creating legacy OCI table: %w", err)
		}
	}

	compositeConstraints := []string{
		"CREATE UNIQUE INDEX IF NOT EXISTS idx_manifests_repo_digest ON manifests (repository_id, digest)",
		"CREATE UNIQUE INDEX IF NOT EXISTS idx_tags_repo_name ON tags (repository_id, name)",
		"CREATE UNIQUE INDEX IF NOT EXISTS idx_peer_blobs_actor_digest ON peer_blobs (peer_actor, blob_digest)",
		"CREATE UNIQUE INDEX IF NOT EXISTS idx_repo_owners_pk ON repository_owners (repository_id, owner_id)",
		"CREATE UNIQUE INDEX IF NOT EXISTS idx_manifest_layers_pk ON manifest_layers (manifest_id, blob_digest)",
		"CREATE UNIQUE INDEX IF NOT EXISTS idx_delivery_queue_activity_inbox ON delivery_queue (activity_id, inbox_url)",
		"CREATE UNIQUE INDEX IF NOT EXISTS idx_actors_actor_url ON actors (actor_url)",
	}
	for _, ddl := range compositeConstraints {
		if _, err := db.bun.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("creating constraint: %w", err)
		}
	}

	indexes := []string{
		"CREATE INDEX IF NOT EXISTS idx_manifests_digest ON manifests (digest)",
		"CREATE INDEX IF NOT EXISTS idx_manifests_repo ON manifests (repository_id)",
		"CREATE INDEX IF NOT EXISTS idx_blobs_stored ON blobs (stored_locally)",
		"CREATE INDEX IF NOT EXISTS idx_peer_blobs_digest ON peer_blobs (blob_digest)",
		"CREATE INDEX IF NOT EXISTS idx_peer_blobs_peer ON peer_blobs (peer_actor)",
		"CREATE INDEX IF NOT EXISTS idx_actors_is_healthy ON actors (is_healthy)",
		"CREATE INDEX IF NOT EXISTS idx_upload_sessions_expires ON upload_sessions (expires_at)",
		"CREATE INDEX IF NOT EXISTS idx_repositories_owner ON repositories (owner_id)",
		"CREATE INDEX IF NOT EXISTS idx_actors_they_follow_us ON actors (they_follow_us)",
		"CREATE INDEX IF NOT EXISTS idx_actors_we_follow_them ON actors (we_follow_them)",
		"CREATE INDEX IF NOT EXISTS idx_actors_we_follow_status ON actors (we_follow_status)",
		"CREATE INDEX IF NOT EXISTS idx_actors_created_at ON actors (created_at)",
		"CREATE INDEX IF NOT EXISTS idx_follow_requests_actor ON follow_requests (actor_url)",
		"CREATE INDEX IF NOT EXISTS idx_activities_type ON activities (type)",
		"CREATE INDEX IF NOT EXISTS idx_activities_actor ON activities (actor_url)",
		"CREATE INDEX IF NOT EXISTS idx_activities_published ON activities (published_at)",
		"CREATE INDEX IF NOT EXISTS idx_delivery_queue_pending ON delivery_queue (status, next_attempt_at)",
		"CREATE INDEX IF NOT EXISTS idx_delivery_queue_activity ON delivery_queue (activity_id)",
		"CREATE INDEX IF NOT EXISTS idx_tags_manifest_digest ON tags (manifest_digest)",
		"CREATE INDEX IF NOT EXISTS idx_manifest_layers_blob_digest ON manifest_layers (blob_digest)",
	}
	for _, ddl := range indexes {
		if _, err := db.bun.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("creating index: %w", err)
		}
	}

	return nil
}

// migrateV3 creates the legacy deleted_manifests table (dropped in v6).
func (db *DB) migrateV3(ctx context.Context) error {
	if _, err := db.bun.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS deleted_manifests (
		digest TEXT PRIMARY KEY,
		repo_name TEXT NOT NULL,
		deleted_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		source_actor TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("creating deleted_manifests table: %w", err)
	}
	if _, err := db.bun.ExecContext(ctx,
		"CREATE INDEX IF NOT EXISTS idx_deleted_manifests_deleted_at ON deleted_manifests (deleted_at)"); err != nil {
		return fmt.Errorf("creating deleted_manifests index: %w", err)
	}
	return nil
}

func (db *DB) migrateV2(ctx context.Context) error {
	tables := []string{"follows", "follow_requests"}
	for _, table := range tables {
		exists, err := db.tableExists(ctx, table)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		if err := db.addColumnIfMissing(ctx, table, "alias", "TEXT"); err != nil {
			return fmt.Errorf("migrateV2: %w", err)
		}
	}
	return nil
}

// migrateV5 consolidates peers, follows, and outgoing_follows into a single actors table.
func (db *DB) migrateV5(ctx context.Context) error {
	// Create the actors table
	if _, err := db.bun.NewCreateTable().Model((*Actor)(nil)).IfNotExists().Exec(ctx); err != nil {
		return fmt.Errorf("creating actors table: %w", err)
	}

	// Create indexes for actors table
	actorIndexes := []string{
		"CREATE UNIQUE INDEX IF NOT EXISTS idx_actors_actor_url ON actors (actor_url)",
		"CREATE INDEX IF NOT EXISTS idx_actors_they_follow_us ON actors (they_follow_us)",
		"CREATE INDEX IF NOT EXISTS idx_actors_we_follow_them ON actors (we_follow_them)",
		"CREATE INDEX IF NOT EXISTS idx_actors_is_healthy ON actors (is_healthy)",
		"CREATE INDEX IF NOT EXISTS idx_actors_we_follow_status ON actors (we_follow_status)",
		"CREATE INDEX IF NOT EXISTS idx_actors_created_at ON actors (created_at)",
	}
	for _, ddl := range actorIndexes {
		if _, err := db.bun.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("creating actors index: %w", err)
		}
	}

	// Migrate data from peers (discovery/health info) - only if table exists
	// This is the base layer with health and replication info
	peersExist, err := db.tableExists(ctx, "peers")
	if err != nil {
		return err
	}
	if peersExist {
		if _, err := db.bun.ExecContext(ctx, `
			INSERT INTO actors (actor_url, name, endpoint, is_healthy, replication_policy, last_seen_at, created_at)
			SELECT actor_url, name, endpoint, is_healthy, replication_policy, last_seen_at, created_at
			FROM peers
			ON CONFLICT(actor_url) DO UPDATE SET
				name = COALESCE(excluded.name, actors.name),
				endpoint = COALESCE(NULLIF(excluded.endpoint, ''), actors.endpoint),
				is_healthy = excluded.is_healthy,
				replication_policy = COALESCE(excluded.replication_policy, actors.replication_policy),
				last_seen_at = COALESCE(excluded.last_seen_at, actors.last_seen_at)
		`); err != nil {
			return fmt.Errorf("migrating peers to actors: %w", err)
		}
	}

	// Migrate data from follows (inbound: they follow us) - only if table exists
	// Merges with existing actor data, preserving peer info
	followsExist, err := db.tableExists(ctx, "follows")
	if err != nil {
		return err
	}
	if followsExist {
		if _, err := db.bun.ExecContext(ctx, `
			INSERT INTO actors (actor_url, endpoint, public_key_pem, alias, they_follow_us, they_follow_us_at)
			SELECT actor_url, endpoint, public_key_pem, alias, TRUE, approved_at
			FROM follows
			ON CONFLICT(actor_url) DO UPDATE SET
				endpoint = COALESCE(NULLIF(excluded.endpoint, ''), actors.endpoint),
				public_key_pem = COALESCE(excluded.public_key_pem, actors.public_key_pem),
				alias = COALESCE(excluded.alias, actors.alias),
				they_follow_us = TRUE,
				they_follow_us_at = COALESCE(excluded.they_follow_us_at, actors.they_follow_us_at)
		`); err != nil {
			return fmt.Errorf("migrating follows to actors: %w", err)
		}
	}

	// Migrate data from outgoing_follows (outbound: we follow them) - only if table exists
	// Merges with existing actor data, preserving peer and follow info
	outgoingExist, err := db.tableExists(ctx, "outgoing_follows")
	if err != nil {
		return err
	}
	if outgoingExist {
		if _, err := db.bun.ExecContext(ctx, `
			INSERT INTO actors (actor_url, endpoint, we_follow_them, we_follow_status, we_follow_accept_at)
			SELECT actor_url, '', TRUE, status, accepted_at
			FROM outgoing_follows
			ON CONFLICT(actor_url) DO UPDATE SET
				we_follow_them = TRUE,
				we_follow_status = COALESCE(excluded.we_follow_status, actors.we_follow_status),
				we_follow_accept_at = COALESCE(excluded.we_follow_accept_at, actors.we_follow_accept_at)
		`); err != nil {
			return fmt.Errorf("migrating outgoing_follows to actors: %w", err)
		}
	}

	// Drop old tables
	dropTables := []string{
		"DROP TABLE IF EXISTS peers",
		"DROP TABLE IF EXISTS follows",
		"DROP TABLE IF EXISTS outgoing_follows",
	}
	for _, ddl := range dropTables {
		if _, err := db.bun.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("dropping old table: %w", err)
		}
	}

	// Drop old indexes that reference dropped tables
	dropIndexes := []string{
		"DROP INDEX IF EXISTS idx_peers_healthy",
		"DROP INDEX IF EXISTS idx_follows_actor",
		"DROP INDEX IF EXISTS idx_outgoing_follows_status",
	}
	for _, ddl := range dropIndexes {
		if _, err := db.bun.ExecContext(ctx, ddl); err != nil {
			// Ignore errors for non-existent indexes
			continue
		}
	}

	return nil
}

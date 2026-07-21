package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

type Artifact struct {
	Key          string
	Upstream     string
	Path         string
	Filename     string
	Size         int64
	ContentType  string
	ETag         string
	LastModified string
	CreatedAt    time.Time
	LastAccessAt time.Time
	Class        string
	Tracked      bool
}

func Open(filename string) (*Store, error) {
	db, err := sql.Open("sqlite", filename)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS artifacts (
			cache_key TEXT PRIMARY KEY,
			upstream_name TEXT NOT NULL,
			artifact_path TEXT NOT NULL,
			filename TEXT NOT NULL,
			size_bytes INTEGER NOT NULL,
			content_type TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			last_access_at INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS artifacts_last_access ON artifacts(last_access_at);
		CREATE TABLE IF NOT EXISTS watches (
			cache_key TEXT PRIMARY KEY,
			last_requested_at INTEGER NOT NULL,
			last_checked_at INTEGER NOT NULL DEFAULT 0,
			FOREIGN KEY(cache_key) REFERENCES artifacts(cache_key) ON DELETE CASCADE
		);
		CREATE INDEX IF NOT EXISTS watches_last_requested ON watches(last_requested_at);
		CREATE INDEX IF NOT EXISTS watches_last_checked ON watches(last_checked_at);
		CREATE TABLE IF NOT EXISTS package_watches (
			backend TEXT NOT NULL,
			upstream_name TEXT NOT NULL,
			package_name TEXT NOT NULL,
			variant TEXT NOT NULL DEFAULT '',
			last_requested_at INTEGER NOT NULL,
			last_checked_at INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (backend, upstream_name, package_name)
		);
		CREATE INDEX IF NOT EXISTS package_watches_due ON package_watches(backend, last_checked_at);
		CREATE TABLE IF NOT EXISTS apt_catalog (
			upstream_name TEXT NOT NULL,
			artifact_path TEXT NOT NULL,
			package_name TEXT NOT NULL,
			version TEXT NOT NULL,
			architecture TEXT NOT NULL DEFAULT '',
			index_path TEXT NOT NULL,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (upstream_name, artifact_path)
		);
		CREATE INDEX IF NOT EXISTS apt_catalog_package ON apt_catalog(upstream_name, package_name);
	`)
	if err != nil {
		return fmt.Errorf("migrate sqlite: %w", err)
	}
	// Version 1 did not retain validators. Add them separately so existing
	// Milestone 1 databases upgrade in place.
	for _, statement := range []string{
		"ALTER TABLE artifacts ADD COLUMN etag TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE artifacts ADD COLUMN last_modified TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE artifacts ADD COLUMN cache_class TEXT NOT NULL DEFAULT 'artifact'",
		"ALTER TABLE artifacts ADD COLUMN tracked INTEGER NOT NULL DEFAULT 1",
		"ALTER TABLE package_watches ADD COLUMN variant TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE apt_catalog ADD COLUMN architecture TEXT NOT NULL DEFAULT ''",
	} {
		if _, err := s.db.Exec(statement); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migrate sqlite: %w", err)
		}
	}
	return nil
}

func (s *Store) Get(key string) (Artifact, bool, error) {
	var a Artifact
	var created, accessed int64
	var tracked int
	err := s.db.QueryRow(`SELECT cache_key, upstream_name, artifact_path, filename, size_bytes, content_type, etag, last_modified, created_at, last_access_at, cache_class, tracked
		FROM artifacts WHERE cache_key = ?`, key).Scan(&a.Key, &a.Upstream, &a.Path, &a.Filename, &a.Size, &a.ContentType, &a.ETag, &a.LastModified, &created, &accessed, &a.Class, &tracked)
	if err == sql.ErrNoRows {
		return Artifact{}, false, nil
	}
	if err != nil {
		return Artifact{}, false, fmt.Errorf("read artifact: %w", err)
	}
	a.CreatedAt = time.Unix(created, 0)
	a.LastAccessAt = time.Unix(accessed, 0)
	a.Tracked = tracked != 0
	return a, true, nil
}

func (s *Store) Complete(a Artifact) error {
	now := time.Now().Unix()
	_, err := s.db.Exec(`INSERT INTO artifacts (cache_key, upstream_name, artifact_path, filename, size_bytes, content_type, etag, last_modified, created_at, last_access_at, cache_class, tracked)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(cache_key) DO UPDATE SET filename=excluded.filename, size_bytes=excluded.size_bytes,
		content_type=excluded.content_type, etag=excluded.etag, last_modified=excluded.last_modified,
		created_at=excluded.created_at, last_access_at=excluded.last_access_at, cache_class=excluded.cache_class, tracked=excluded.tracked`,
		a.Key, a.Upstream, a.Path, a.Filename, a.Size, a.ContentType, a.ETag, a.LastModified, now, now, a.Class, boolToInt(a.Tracked))
	if err != nil {
		return fmt.Errorf("record completed artifact: %w", err)
	}
	return nil
}

func (s *Store) Revalidated(key string) error {
	now := time.Now().Unix()
	_, err := s.db.Exec("UPDATE artifacts SET created_at = ?, last_access_at = ? WHERE cache_key = ?", now, now, key)
	return err
}

func (s *Store) Touch(key string) error {
	_, err := s.db.Exec("UPDATE artifacts SET last_access_at = ? WHERE cache_key = ?", time.Now().Unix(), key)
	return err
}

type EvictionCandidate struct {
	Key      string
	Filename string
	Size     int64
}

// Watch is an artifact that a client actually requested successfully. It is
// deliberately artifact-based: repository metadata alone never creates one.
type Watch struct {
	Artifact
	LastRequestedAt time.Time
	LastCheckedAt   time.Time
}

func (s *Store) Watch(key string) error {
	now := time.Now().Unix()
	_, err := s.db.Exec(`INSERT INTO watches (cache_key, last_requested_at, last_checked_at)
		VALUES (?, ?, 0)
		ON CONFLICT(cache_key) DO UPDATE SET last_requested_at = excluded.last_requested_at`, key, now)
	return err
}

func (s *Store) WatchesDue(before, activeSince time.Time) ([]Watch, error) {
	rows, err := s.db.Query(`SELECT a.cache_key, a.upstream_name, a.artifact_path, a.filename, a.size_bytes,
		a.content_type, a.etag, a.last_modified, a.created_at, a.last_access_at, a.cache_class, a.tracked,
		w.last_requested_at, w.last_checked_at
		FROM watches w JOIN artifacts a ON a.cache_key = w.cache_key
		WHERE w.last_requested_at >= ? AND w.last_checked_at < ? ORDER BY w.last_checked_at ASC`, activeSince.Unix(), before.Unix())
	if err != nil {
		return nil, fmt.Errorf("list due watches: %w", err)
	}
	defer rows.Close()
	var watches []Watch
	for rows.Next() {
		var watch Watch
		var created, accessed, requested, checked int64
		var tracked int
		if err := rows.Scan(&watch.Key, &watch.Upstream, &watch.Path, &watch.Filename, &watch.Size, &watch.ContentType,
			&watch.ETag, &watch.LastModified, &created, &accessed, &watch.Class, &tracked, &requested, &checked); err != nil {
			return nil, fmt.Errorf("read watch: %w", err)
		}
		watch.CreatedAt = time.Unix(created, 0)
		watch.LastAccessAt = time.Unix(accessed, 0)
		watch.LastRequestedAt = time.Unix(requested, 0)
		watch.LastCheckedAt = time.Unix(checked, 0)
		watch.Tracked = tracked != 0
		watches = append(watches, watch)
	}
	return watches, rows.Err()
}

func (s *Store) CheckedWatch(key string) error {
	_, err := s.db.Exec("UPDATE watches SET last_checked_at = ? WHERE cache_key = ?", time.Now().Unix(), key)
	return err
}

func (s *Store) DeleteInactiveWatches(before time.Time) (int64, error) {
	result, err := s.db.Exec("DELETE FROM watches WHERE last_requested_at < ?", before.Unix())
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return count, nil
}

type PackageWatch struct {
	Backend         string
	Upstream        string
	Package         string
	Variant         string
	LastRequestedAt time.Time
	LastCheckedAt   time.Time
}

type APTRecord struct {
	ArtifactPath string
	Package      string
	Version      string
	Architecture string
}

func (s *Store) ReplaceAPTCatalog(upstream, indexPath string, records []APTRecord) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM apt_catalog WHERE upstream_name = ? AND index_path = ?", upstream, indexPath); err != nil {
		return err
	}
	statement, err := tx.Prepare(`INSERT INTO apt_catalog (upstream_name, artifact_path, package_name, version, architecture, index_path, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(upstream_name, artifact_path) DO UPDATE SET package_name=excluded.package_name,
		version=excluded.version, architecture=excluded.architecture, index_path=excluded.index_path, updated_at=excluded.updated_at`)
	if err != nil {
		return err
	}
	defer statement.Close()
	now := time.Now().Unix()
	for _, record := range records {
		if _, err := statement.Exec(upstream, record.ArtifactPath, record.Package, record.Version, record.Architecture, indexPath, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) APTRecordForArtifact(upstream, artifactPath string) (APTRecord, bool, error) {
	var record APTRecord
	err := s.db.QueryRow(`SELECT artifact_path, package_name, version, architecture FROM apt_catalog
		WHERE upstream_name = ? AND artifact_path = ?`, upstream, artifactPath).Scan(&record.ArtifactPath, &record.Package, &record.Version, &record.Architecture)
	if err == sql.ErrNoRows {
		return APTRecord{}, false, nil
	}
	if err != nil {
		return APTRecord{}, false, err
	}
	return record, true, nil
}

func (s *Store) APTRecords(upstream, packageName string) ([]APTRecord, error) {
	rows, err := s.db.Query(`SELECT artifact_path, package_name, version, architecture FROM apt_catalog
		WHERE upstream_name = ? AND package_name = ?`, upstream, packageName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []APTRecord
	for rows.Next() {
		var record APTRecord
		if err := rows.Scan(&record.ArtifactPath, &record.Package, &record.Version, &record.Architecture); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func (s *Store) WatchPackage(backend, upstream, packageName string) error {
	return s.WatchPackageVariant(backend, upstream, packageName, "")
}

func (s *Store) WatchPackageVariant(backend, upstream, packageName, variant string) error {
	now := time.Now().Unix()
	_, err := s.db.Exec(`INSERT INTO package_watches (backend, upstream_name, package_name, variant, last_requested_at, last_checked_at)
		VALUES (?, ?, ?, ?, ?, 0)
		ON CONFLICT(backend, upstream_name, package_name) DO UPDATE SET variant = excluded.variant, last_requested_at = excluded.last_requested_at`, backend, upstream, packageName, variant, now)
	return err
}

func (s *Store) PackageWatchesDue(backend string, before, activeSince time.Time) ([]PackageWatch, error) {
	rows, err := s.db.Query(`SELECT backend, upstream_name, package_name, variant, last_requested_at, last_checked_at
		FROM package_watches WHERE backend = ? AND last_requested_at >= ? AND last_checked_at < ?
		ORDER BY last_checked_at ASC`, backend, activeSince.Unix(), before.Unix())
	if err != nil {
		return nil, fmt.Errorf("list due package watches: %w", err)
	}
	defer rows.Close()
	var watches []PackageWatch
	for rows.Next() {
		var watch PackageWatch
		var requested, checked int64
		if err := rows.Scan(&watch.Backend, &watch.Upstream, &watch.Package, &watch.Variant, &requested, &checked); err != nil {
			return nil, fmt.Errorf("read package watch: %w", err)
		}
		watch.LastRequestedAt = time.Unix(requested, 0)
		watch.LastCheckedAt = time.Unix(checked, 0)
		watches = append(watches, watch)
	}
	return watches, rows.Err()
}

func (s *Store) CheckedPackageWatch(backend, upstream, packageName string) error {
	_, err := s.db.Exec(`UPDATE package_watches SET last_checked_at = ?
		WHERE backend = ? AND upstream_name = ? AND package_name = ?`, time.Now().Unix(), backend, upstream, packageName)
	return err
}

func (s *Store) DeleteInactivePackageWatches(before time.Time) (int64, error) {
	result, err := s.db.Exec("DELETE FROM package_watches WHERE last_requested_at < ?", before.Unix())
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

type Stats struct {
	Entries        int64
	TrackedEntries int64
	SizeBytes      int64
}

func (s *Store) Stats() (Stats, error) {
	var stats Stats
	err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(tracked), 0), COALESCE(SUM(size_bytes), 0) FROM artifacts`).Scan(&stats.Entries, &stats.TrackedEntries, &stats.SizeBytes)
	return stats, err
}

func (s *Store) InactiveTracked(before time.Time) ([]EvictionCandidate, error) {
	return s.candidates(`SELECT cache_key, filename, size_bytes FROM artifacts
		WHERE tracked = 1 AND last_access_at < ? ORDER BY last_access_at ASC`, before.Unix())
}

func (s *Store) LeastRecentlyUsed() ([]EvictionCandidate, error) {
	return s.candidates(`SELECT cache_key, filename, size_bytes FROM artifacts
		ORDER BY last_access_at ASC`)
}

func (s *Store) candidates(query string, args ...any) ([]EvictionCandidate, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list eviction candidates: %w", err)
	}
	defer rows.Close()
	var result []EvictionCandidate
	for rows.Next() {
		var candidate EvictionCandidate
		if err := rows.Scan(&candidate.Key, &candidate.Filename, &candidate.Size); err != nil {
			return nil, fmt.Errorf("read eviction candidate: %w", err)
		}
		result = append(result, candidate)
	}
	return result, rows.Err()
}

func (s *Store) TotalSize() (int64, error) {
	var size int64
	if err := s.db.QueryRow("SELECT COALESCE(SUM(size_bytes), 0) FROM artifacts").Scan(&size); err != nil {
		return 0, fmt.Errorf("calculate cache size: %w", err)
	}
	return size, nil
}

func (s *Store) Delete(key string) error {
	if _, err := s.db.Exec("DELETE FROM watches WHERE cache_key = ?", key); err != nil {
		return err
	}
	_, err := s.db.Exec("DELETE FROM artifacts WHERE cache_key = ?", key)
	return err
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (s *Store) Close() error { return s.db.Close() }

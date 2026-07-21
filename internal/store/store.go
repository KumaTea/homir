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

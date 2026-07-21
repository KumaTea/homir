package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

type Artifact struct {
	Key         string
	Upstream    string
	Path        string
	Filename    string
	Size        int64
	ContentType string
	CreatedAt   time.Time
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
	return nil
}

func (s *Store) Get(key string) (Artifact, bool, error) {
	var a Artifact
	var created int64
	err := s.db.QueryRow(`SELECT cache_key, upstream_name, artifact_path, filename, size_bytes, content_type, created_at
		FROM artifacts WHERE cache_key = ?`, key).Scan(&a.Key, &a.Upstream, &a.Path, &a.Filename, &a.Size, &a.ContentType, &created)
	if err == sql.ErrNoRows {
		return Artifact{}, false, nil
	}
	if err != nil {
		return Artifact{}, false, fmt.Errorf("read artifact: %w", err)
	}
	a.CreatedAt = time.Unix(created, 0)
	return a, true, nil
}

func (s *Store) Complete(a Artifact) error {
	now := time.Now().Unix()
	_, err := s.db.Exec(`INSERT INTO artifacts (cache_key, upstream_name, artifact_path, filename, size_bytes, content_type, created_at, last_access_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(cache_key) DO UPDATE SET filename=excluded.filename, size_bytes=excluded.size_bytes,
		content_type=excluded.content_type, last_access_at=excluded.last_access_at`,
		a.Key, a.Upstream, a.Path, a.Filename, a.Size, a.ContentType, now, now)
	if err != nil {
		return fmt.Errorf("record completed artifact: %w", err)
	}
	return nil
}

func (s *Store) Touch(key string) error {
	_, err := s.db.Exec("UPDATE artifacts SET last_access_at = ? WHERE cache_key = ?", time.Now().Unix(), key)
	return err
}

func (s *Store) Close() error { return s.db.Close() }

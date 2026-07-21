package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/KumaTea/homir/internal/config"
	"github.com/KumaTea/homir/internal/store"
)

type Manager struct {
	root      string
	artifacts string
	partial   string
	upstreams map[string]config.Upstream
	store     *store.Store
	client    *http.Client
	logger    *slog.Logger

	mu       sync.Mutex
	sessions map[string]*session
}

// Policy distinguishes durable package artifacts from short-lived repository
// metadata while using the same streaming and disk-cache machinery.
type Policy struct {
	Namespace    string
	RefreshAfter time.Duration
}

var ArtifactPolicy = Policy{Namespace: "artifact"}

type session struct {
	key      string
	partPath string
	filePath string
	prior    *store.Artifact
	policy   Policy

	mu          sync.Mutex
	changed     *sync.Cond
	ready       chan struct{}
	done        chan struct{}
	available   int64
	total       int64
	contentType string
	complete    bool
	err         error
}

func New(root string, upstreams map[string]config.Upstream, db *store.Store, logger *slog.Logger) (*Manager, error) {
	artifacts := filepath.Join(root, "artifacts")
	partial := filepath.Join(root, "partial")
	for _, dir := range []string{artifacts, partial} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create cache directory: %w", err)
		}
	}
	// Partial files are never safe cache hits after a restart.
	if entries, err := os.ReadDir(partial); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				_ = os.Remove(filepath.Join(partial, entry.Name()))
			}
		}
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	m := &Manager{
		root: root, artifacts: artifacts, partial: partial, upstreams: upstreams, store: db,
		client: &http.Client{Transport: transport}, logger: logger, sessions: make(map[string]*session),
	}
	return m, nil
}

// Serve proxies one configured upstream artifact. It is deliberately protocol
// neutral; future package backends will call this after their own path parsing.
func (m *Manager) Serve(w http.ResponseWriter, r *http.Request, upstreamName, artifactPath string) {
	m.ServeWithPolicy(w, r, upstreamName, artifactPath, ArtifactPolicy)
}

// ServeWithPolicy proxies one configured upstream path under a cache policy.
func (m *Manager) ServeWithPolicy(w http.ResponseWriter, r *http.Request, upstreamName, artifactPath string, policy Policy) {
	if !validArtifactPath(artifactPath) {
		http.Error(w, "invalid artifact path", http.StatusBadRequest)
		return
	}
	upstream, ok := m.upstreams[upstreamName]
	if !ok {
		http.NotFound(w, r)
		return
	}

	if policy.Namespace == "" {
		policy.Namespace = "artifact"
	}
	key := cacheKey(policy.Namespace, upstreamName, artifactPath)
	var prior *store.Artifact
	if artifact, found, err := m.store.Get(key); err != nil {
		http.Error(w, "cache metadata error", http.StatusInternalServerError)
		return
	} else if found {
		filename := filepath.Join(m.artifacts, artifact.Filename)
		if file, err := os.Open(filename); err == nil {
			if policy.RefreshAfter > 0 && time.Since(artifact.CreatedAt) >= policy.RefreshAfter {
				_ = file.Close()
				prior = &artifact
			} else {
				defer file.Close()
				w.Header().Set("Content-Type", artifact.ContentType)
				http.ServeContent(w, r, pathpkg.Base(artifactPath), artifact.CreatedAt, file)
				_ = m.store.Touch(key)
				return
			}
		}
		// A cache directory may be manually cleaned. Treat a missing object as a miss.
	}

	s := m.getOrStart(key, upstreamName, artifactPath, upstream, prior, policy)
	m.serveSession(w, r, s)
}

func (m *Manager) getOrStart(key, upstreamName, artifactPath string, upstream config.Upstream, prior *store.Artifact, policy Policy) *session {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.sessions[key]; s != nil {
		return s
	}
	partPath := filepath.Join(m.partial, key+".part")
	s := &session{key: key, partPath: partPath, filePath: partPath, prior: prior, policy: policy, total: -1, ready: make(chan struct{}), done: make(chan struct{})}
	s.changed = sync.NewCond(&s.mu)
	m.sessions[key] = s
	go m.download(s, upstreamName, artifactPath, upstream)
	return s
}

func (m *Manager) download(s *session, upstreamName, artifactPath string, upstream config.Upstream) {
	defer func() {
		m.mu.Lock()
		delete(m.sessions, s.key)
		m.mu.Unlock()
	}()

	file, err := os.OpenFile(s.partPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		s.finish(err)
		return
	}
	defer file.Close()

	response, err := m.fetch(upstream, artifactPath, s.prior)
	if err != nil {
		s.finish(err)
		_ = os.Remove(s.partPath)
		return
	}
	defer response.Body.Close()

	if response.StatusCode == http.StatusNotModified {
		if s.prior == nil {
			s.finish(fmt.Errorf("upstream returned 304 without a cached object"))
			_ = os.Remove(s.partPath)
			return
		}
		_ = os.Remove(s.partPath)
		s.setCachedFile(filepath.Join(m.artifacts, s.prior.Filename), s.prior.Size, s.prior.ContentType)
		if err := m.store.Revalidated(s.key); err != nil {
			s.finish(err)
			return
		}
		s.finish(nil)
		return
	}

	s.setHeaders(response.ContentLength, response.Header.Get("Content-Type"))
	buffer := make([]byte, 128*1024)
	for {
		n, readErr := response.Body.Read(buffer)
		if n > 0 {
			if _, err := file.Write(buffer[:n]); err != nil {
				s.finish(err)
				_ = os.Remove(s.partPath)
				return
			}
			s.addAvailable(int64(n))
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			s.finish(readErr)
			_ = os.Remove(s.partPath)
			return
		}
	}
	if err := file.Sync(); err != nil {
		s.finish(err)
		_ = os.Remove(s.partPath)
		return
	}

	filename := s.key + ".artifact"
	finalPath := filepath.Join(m.artifacts, filename)
	if err := os.Rename(s.partPath, finalPath); err != nil {
		s.finish(err)
		return
	}
	s.setFilePath(finalPath)
	s.mu.Lock()
	size, contentType := s.available, s.contentType
	s.mu.Unlock()
	if err := m.store.Complete(store.Artifact{Key: s.key, Upstream: upstreamName, Path: artifactPath, Filename: filename, Size: size, ContentType: contentType, ETag: response.Header.Get("ETag"), LastModified: response.Header.Get("Last-Modified")}); err != nil {
		s.finish(err)
		return
	}
	s.finish(nil)
}

func (m *Manager) fetch(upstream config.Upstream, artifactPath string, prior *store.Artifact) (*http.Response, error) {
	urls := append([]string{upstream.Primary}, upstream.Backups...)
	var lastErr error
	for _, base := range urls {
		target, err := joinURL(base, artifactPath)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequest(http.MethodGet, target, nil)
		if err != nil {
			return nil, err
		}
		if prior != nil {
			if prior.ETag != "" {
				req.Header.Set("If-None-Match", prior.ETag)
			} else if prior.LastModified != "" {
				req.Header.Set("If-Modified-Since", prior.LastModified)
			}
		}
		resp, err := m.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("%s returned %s", target, resp.Status)
			resp.Body.Close()
			continue
		}
		if resp.StatusCode == http.StatusNotModified && prior != nil {
			return resp, nil
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("%s returned %s", target, resp.Status)
		}
		return resp, nil
	}
	return nil, fmt.Errorf("all upstreams failed: %w", lastErr)
}

func (m *Manager) serveSession(w http.ResponseWriter, r *http.Request, s *session) {
	select {
	case <-s.ready:
	case <-s.done:
	}

	s.mu.Lock()
	err, total, contentType, filePath := s.err, s.total, s.contentType, s.filePath
	s.mu.Unlock()
	if err != nil {
		http.Error(w, "upstream download failed", http.StatusBadGateway)
		return
	}

	start, end, partial, err := parseRange(r.Header.Get("Range"), total)
	if err != nil {
		if total >= 0 {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", total))
		}
		http.Error(w, "invalid range", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	if partial {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", end-start+1))
		w.WriteHeader(http.StatusPartialContent)
	} else if total >= 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", total))
	}

	file, err := os.Open(filePath)
	if err != nil {
		http.Error(w, "cache temporary file unavailable", http.StatusInternalServerError)
		return
	}
	defer file.Close()
	if err := streamGrowingFile(w, file, s, start, end); err != nil && !errors.Is(err, net.ErrClosed) {
		m.logger.Debug("stream shared download", "error", err, "key", s.key)
	}
}

func streamGrowingFile(w http.ResponseWriter, file *os.File, s *session, start, end int64) error {
	offset := start
	buffer := make([]byte, 64*1024)
	for end < 0 || offset <= end {
		s.mu.Lock()
		for offset >= s.available && !s.complete && s.err == nil {
			s.changed.Wait()
		}
		available, complete, terminalErr := s.available, s.complete, s.err
		s.mu.Unlock()
		if offset >= available {
			if terminalErr != nil {
				return terminalErr
			}
			if complete {
				return nil
			}
			return nil
		}
		limit := available
		if end >= 0 && limit > end+1 {
			limit = end + 1
		}
		for offset < limit {
			count := int64(len(buffer))
			if remaining := limit - offset; count > remaining {
				count = remaining
			}
			n, err := file.ReadAt(buffer[:count], offset)
			if n > 0 {
				if _, writeErr := w.Write(buffer[:n]); writeErr != nil {
					return writeErr
				}
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				offset += int64(n)
			}
			if err != nil && !errors.Is(err, io.EOF) {
				return err
			}
		}
	}
	return nil
}

func (s *session) setHeaders(total int64, contentType string) {
	s.mu.Lock()
	s.total, s.contentType = total, contentType
	select {
	case <-s.ready:
	default:
		close(s.ready)
	}
	s.mu.Unlock()
}

func (s *session) setCachedFile(path string, size int64, contentType string) {
	s.mu.Lock()
	s.filePath, s.total, s.available, s.contentType = path, size, size, contentType
	select {
	case <-s.ready:
	default:
		close(s.ready)
	}
	s.changed.Broadcast()
	s.mu.Unlock()
}

func (s *session) setFilePath(path string) {
	s.mu.Lock()
	s.filePath = path
	s.mu.Unlock()
}

func (s *session) addAvailable(n int64) {
	s.mu.Lock()
	s.available += n
	s.changed.Broadcast()
	s.mu.Unlock()
}

func (s *session) finish(err error) {
	s.mu.Lock()
	s.err = err
	s.complete = true
	select {
	case <-s.ready:
	default:
		close(s.ready)
	}
	select {
	case <-s.done:
	default:
		close(s.done)
	}
	s.changed.Broadcast()
	s.mu.Unlock()
}

func cacheKey(namespace, upstream, artifactPath string) string {
	sum := sha256.Sum256([]byte(namespace + "\n" + upstream + "\n" + artifactPath))
	return hex.EncodeToString(sum[:])
}

func validArtifactPath(value string) bool {
	if value == "" || strings.HasPrefix(value, "/") {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func joinURL(base, artifactPath string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + artifactPath
	return u.String(), nil
}

func parseRange(header string, total int64) (start, end int64, partial bool, err error) {
	if header == "" {
		return 0, -1, false, nil
	}
	if total < 0 || !strings.HasPrefix(header, "bytes=") || strings.Contains(header, ",") {
		return 0, 0, false, fmt.Errorf("unsupported range")
	}
	value := strings.TrimPrefix(header, "bytes=")
	parts := strings.SplitN(value, "-", 2)
	if len(parts) != 2 || parts[0] == "" {
		return 0, 0, false, fmt.Errorf("invalid range")
	}
	if _, err := fmt.Sscan(parts[0], &start); err != nil || start < 0 || start >= total {
		return 0, 0, false, fmt.Errorf("invalid range")
	}
	end = total - 1
	if parts[1] != "" {
		if _, err := fmt.Sscan(parts[1], &end); err != nil || end < start {
			return 0, 0, false, fmt.Errorf("invalid range")
		}
		if end >= total {
			end = total - 1
		}
	}
	return start, end, true, nil
}

package cache

import (
	"context"
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
	lifecycle config.LifecycleSettings

	mu       sync.Mutex
	sessions map[string]*session
}

// Policy distinguishes durable package artifacts from short-lived repository
// metadata while using the same streaming and disk-cache machinery.
type Policy struct {
	Namespace    string
	RefreshAfter time.Duration
	Track        bool
}

var ArtifactPolicy = Policy{Namespace: "artifact", Track: true}

type session struct {
	key       string
	partPath  string
	filePath  string
	prior     *store.Artifact
	policy    Policy
	sourceURL string
	cancel    context.CancelFunc

	mu          sync.Mutex
	changed     *sync.Cond
	ready       chan struct{}
	done        chan struct{}
	available   int64
	total       int64
	contentType string
	complete    bool
	err         error
	readers     int
	background  bool
	idleTimer   *time.Timer
}

func New(root string, upstreams map[string]config.Upstream, lifecycle config.LifecycleSettings, db *store.Store, logger *slog.Logger) (*Manager, error) {
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
		client: &http.Client{Transport: transport}, logger: logger, lifecycle: lifecycle, sessions: make(map[string]*session),
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
	m.serveResolved(w, r, upstreamName, artifactPath, upstream, policy, "")
	if policy.Track && m.Cached(upstreamName, artifactPath, policy) {
		_ = m.Watch(upstreamName, artifactPath, policy)
	}
}

// ServeURL caches a signed, backend-approved artifact URL. It is not an open
// proxy: callers are responsible for validating the URL before this method is
// reached, and its identity is bound to the configured upstream name.
func (m *Manager) ServeURL(w http.ResponseWriter, r *http.Request, upstreamName, cachePath, sourceURL string, policy Policy) {
	u, err := url.Parse(sourceURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		http.Error(w, "invalid artifact URL", http.StatusBadRequest)
		return
	}
	upstream, ok := m.upstreams[upstreamName]
	if !ok {
		http.NotFound(w, r)
		return
	}
	m.serveResolved(w, r, upstreamName, cachePath, upstream, policy, u.String())
	if policy.Track && m.Cached(upstreamName, cachePath, policy) {
		_ = m.Watch(upstreamName, cachePath, policy)
	}
}

func (m *Manager) Cached(upstreamName, artifactPath string, policy Policy) bool {
	if policy.Namespace == "" {
		policy.Namespace = "artifact"
	}
	_, found, err := m.store.Get(cacheKey(policy.Namespace, upstreamName, artifactPath))
	return err == nil && found
}

// OpenCached returns a completed cache object for backend metadata parsing.
// Callers must close the returned file and must not modify it.
func (m *Manager) OpenCached(upstreamName, artifactPath string, policy Policy) (*os.File, error) {
	if policy.Namespace == "" {
		policy.Namespace = "artifact"
	}
	artifact, found, err := m.store.Get(cacheKey(policy.Namespace, upstreamName, artifactPath))
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, os.ErrNotExist
	}
	return os.Open(filepath.Join(m.artifacts, artifact.Filename))
}

func (m *Manager) serveResolved(w http.ResponseWriter, r *http.Request, upstreamName, artifactPath string, upstream config.Upstream, policy Policy, sourceURL string) {
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

	s := m.getOrStart(key, upstreamName, artifactPath, upstream, prior, policy, sourceURL)
	m.serveSession(w, r, s)
}

func (m *Manager) getOrStart(key, upstreamName, artifactPath string, upstream config.Upstream, prior *store.Artifact, policy Policy, sourceURL string) *session {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.sessions[key]; s != nil {
		return s
	}
	partPath := filepath.Join(m.partial, key+".part")
	ctx, cancel := context.WithCancel(context.Background())
	s := &session{key: key, partPath: partPath, filePath: partPath, prior: prior, policy: policy, sourceURL: sourceURL, cancel: cancel, total: -1, ready: make(chan struct{}), done: make(chan struct{})}
	s.changed = sync.NewCond(&s.mu)
	m.sessions[key] = s
	go m.download(ctx, s, upstreamName, artifactPath, upstream)
	return s
}

func (m *Manager) download(ctx context.Context, s *session, upstreamName, artifactPath string, upstream config.Upstream) {
	defer s.cancel()
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

	var response *http.Response
	if s.sourceURL != "" {
		response, err = m.fetchURL(ctx, s.sourceURL, s.prior)
	} else {
		response, err = m.fetch(ctx, upstream, artifactPath, s.prior)
	}
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
	if err := m.store.Complete(store.Artifact{Key: s.key, Upstream: upstreamName, Path: artifactPath, Filename: filename, Size: size, ContentType: contentType, ETag: response.Header.Get("ETag"), LastModified: response.Header.Get("Last-Modified"), Class: s.policy.Namespace, Tracked: s.policy.Track}); err != nil {
		m.logger.Error("record completed cache entry", "key", s.key, "error", err)
		s.finish(err)
		return
	}
	s.finish(nil)
}

func (m *Manager) fetch(ctx context.Context, upstream config.Upstream, artifactPath string, prior *store.Artifact) (*http.Response, error) {
	urls := append([]string{upstream.Primary}, upstream.Backups...)
	var lastErr error
	for _, base := range urls {
		target, err := joinURL(base, artifactPath)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
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

func (m *Manager) fetchURL(ctx context.Context, sourceURL string, prior *store.Artifact) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
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
		return nil, err
	}
	if resp.StatusCode == http.StatusNotModified && prior != nil {
		return resp, nil
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("%s returned %s", sourceURL, resp.Status)
	}
	return resp, nil
}

func (m *Manager) serveSession(w http.ResponseWriter, r *http.Request, s *session) {
	s.addReader(m.lifecycle.PartialTTL)
	defer s.removeReader(m.lifecycle.PartialTTL)
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
	if err := streamGrowingFile(r.Context(), w, file, s, start, end); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, context.Canceled) {
		m.logger.Debug("stream shared download", "error", err, "key", s.key)
	}
}

func (s *session) addReader(_ time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readers++
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
}

func (s *session) removeReader(ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readers > 0 {
		s.readers--
	}
	if s.readers != 0 || s.complete || s.background || ttl <= 0 {
		return
	}
	s.idleTimer = time.AfterFunc(ttl, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.readers == 0 && !s.complete {
			s.cancel()
		}
	})
}

func (s *session) keepAlive() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.background = true
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
}

func streamGrowingFile(ctx context.Context, w http.ResponseWriter, file *os.File, s *session, start, end int64) error {
	stopContextWake := make(chan struct{})
	defer close(stopContextWake)
	go func() {
		select {
		case <-ctx.Done():
			s.mu.Lock()
			s.changed.Broadcast()
			s.mu.Unlock()
		case <-stopContextWake:
		}
	}()
	offset := start
	buffer := make([]byte, 64*1024)
	for end < 0 || offset <= end {
		s.mu.Lock()
		for offset >= s.available && !s.complete && s.err == nil {
			if err := ctx.Err(); err != nil {
				s.mu.Unlock()
				return err
			}
			s.changed.Wait()
		}
		available, complete, terminalErr := s.available, s.complete, s.err
		s.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return err
		}
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
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
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

type CleanupResult struct {
	RemovedEntries int
	ReleasedBytes  int64
	SkippedActive  int
}

type WatchRefreshResult struct {
	Due     int
	Started int
	Expired int64
}

// Watch records a real artifact request. Backends call it only after handing
// an artifact response to a client; indexes and repository metadata do not
// enter the watch list.
func (m *Manager) Watch(upstreamName, artifactPath string, policy Policy) error {
	if policy.Namespace == "" {
		policy.Namespace = "artifact"
	}
	return m.store.Watch(cacheKey(policy.Namespace, upstreamName, artifactPath))
}

func (m *Manager) WatchPackage(backend, upstreamName, packageName string) error {
	return m.store.WatchPackage(backend, upstreamName, packageName)
}

// PrefetchURL starts a backend-approved artifact download without a client.
// The normal session map still coalesces it with a client request for the same
// object, and a completed object is conditionally revalidated on later runs.
func (m *Manager) PrefetchURL(upstreamName, sourceURL string) error {
	u, err := url.Parse(sourceURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("invalid prefetch URL")
	}
	upstream, ok := m.upstreams[upstreamName]
	if !ok {
		return fmt.Errorf("unknown upstream %q", upstreamName)
	}
	key := cacheKey(ArtifactPolicy.Namespace, upstreamName, sourceURL)
	artifact, found, err := m.store.Get(key)
	if err != nil {
		return err
	}
	var prior *store.Artifact
	if found {
		prior = &artifact
	}
	m.getOrStart(key, upstreamName, sourceURL, upstream, prior, ArtifactPolicy, sourceURL).keepAlive()
	return nil
}

func (m *Manager) Prefetch(upstreamName, artifactPath string) error {
	if !validArtifactPath(artifactPath) {
		return fmt.Errorf("invalid prefetch artifact path")
	}
	upstream, ok := m.upstreams[upstreamName]
	if !ok {
		return fmt.Errorf("unknown upstream %q", upstreamName)
	}
	key := cacheKey(ArtifactPolicy.Namespace, upstreamName, artifactPath)
	artifact, found, err := m.store.Get(key)
	if err != nil {
		return err
	}
	var prior *store.Artifact
	if found {
		prior = &artifact
	}
	m.getOrStart(key, upstreamName, artifactPath, upstream, prior, ArtifactPolicy, "").keepAlive()
	return nil
}

// RefreshWatches starts conditional refreshes for active watched artifacts.
// It intentionally refreshes known artifacts only. Discovering additional
// versions is a protocol-specific operation performed by backend workers.
func (m *Manager) RefreshWatches(interval, retention time.Duration) (WatchRefreshResult, error) {
	if interval <= 0 || retention <= 0 {
		return WatchRefreshResult{}, fmt.Errorf("watch interval and retention must be positive")
	}
	now := time.Now()
	result := WatchRefreshResult{}
	var err error
	result.Expired, err = m.store.DeleteInactiveWatches(now.Add(-retention))
	if err != nil {
		return result, err
	}
	watches, err := m.store.WatchesDue(now.Add(-interval), now.Add(-retention))
	if err != nil {
		return result, err
	}
	result.Due = len(watches)
	for _, watch := range watches {
		upstream, ok := m.upstreams[watch.Upstream]
		if !ok {
			continue
		}
		policy := Policy{Namespace: watch.Class, Track: watch.Tracked}
		sourceURL := ""
		if parsed, err := url.Parse(watch.Path); err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != "" {
			sourceURL = watch.Path
		}
		m.getOrStart(watch.Key, watch.Upstream, watch.Path, upstream, &watch.Artifact, policy, sourceURL)
		if err := m.store.CheckedWatch(watch.Key); err != nil {
			return result, err
		}
		result.Started++
	}
	return result, nil
}

func (m *Manager) StartWatchRefresh(ctx context.Context, interval, retention time.Duration) {
	go func() {
		if _, err := m.RefreshWatches(interval, retention); err != nil {
			m.logger.Error("refresh watched artifacts", "error", err)
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := m.RefreshWatches(interval, retention); err != nil {
					m.logger.Error("refresh watched artifacts", "error", err)
				}
			}
		}
	}()
}

// StartCleanup launches the periodic lifecycle worker. It does not delete
// partial files or entries with an active shared-download session.
func (m *Manager) StartCleanup(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(m.lifecycle.CleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := m.Cleanup(); err != nil {
					m.logger.Error("cache cleanup failed", "error", err)
				}
			}
		}
	}()
}

func (m *Manager) Cleanup() (CleanupResult, error) {
	var result CleanupResult
	inactive, err := m.store.InactiveTracked(time.Now().Add(-m.lifecycle.InactivityTTL))
	if err != nil {
		return result, err
	}
	for _, candidate := range inactive {
		m.removeCandidate(candidate, &result)
	}

	total, err := m.store.TotalSize()
	if err != nil {
		return result, err
	}
	if total <= m.lifecycle.MaxSize {
		return result, nil
	}
	candidates, err := m.store.LeastRecentlyUsed()
	if err != nil {
		return result, err
	}
	for _, candidate := range candidates {
		if total <= m.lifecycle.MaxSize {
			break
		}
		if m.removeCandidate(candidate, &result) {
			total -= candidate.Size
		}
	}
	return result, nil
}

func (m *Manager) removeCandidate(candidate store.EvictionCandidate, result *CleanupResult) bool {
	m.mu.Lock()
	_, active := m.sessions[candidate.Key]
	m.mu.Unlock()
	if active {
		result.SkippedActive++
		return false
	}
	filename := filepath.Join(m.artifacts, candidate.Filename)
	if err := os.Remove(filename); err != nil && !errors.Is(err, os.ErrNotExist) {
		m.logger.Warn("remove cached artifact", "key", candidate.Key, "error", err)
		return false
	}
	if err := m.store.Delete(candidate.Key); err != nil {
		m.logger.Warn("remove cached artifact metadata", "key", candidate.Key, "error", err)
		return false
	}
	result.RemovedEntries++
	result.ReleasedBytes += candidate.Size
	return true
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

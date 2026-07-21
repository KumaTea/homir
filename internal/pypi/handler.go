// Package pypi implements the PyPI Simple API-facing part of Homir.
package pypi

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/KumaTea/homir/internal/cache"
	"github.com/KumaTea/homir/internal/config"
	"github.com/KumaTea/homir/internal/store"
)

type Handler struct {
	cache     *cache.Manager
	store     *store.Store
	upstreams map[string]config.Upstream
	secret    []byte
	client    *http.Client
}

func NewHandler(manager *cache.Manager, db *store.Store, upstreams map[string]config.Upstream, dataDirectory string) (*Handler, error) {
	secret, err := loadSecret(filepath.Join(dataDirectory, "pypi-link-signing.key"))
	if err != nil {
		return nil, err
	}
	return &Handler{cache: manager, store: db, upstreams: upstreams, secret: secret, client: http.DefaultClient}, nil
}

func loadSecret(filename string) ([]byte, error) {
	if secret, err := os.ReadFile(filename); err == nil {
		if len(secret) != 32 {
			return nil, fmt.Errorf("invalid PyPI link-signing key length")
		}
		return secret, nil
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read PyPI link-signing key: %w", err)
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("create PyPI link-signing key: %w", err)
	}
	if err := os.WriteFile(filename, secret, 0o600); err != nil {
		return nil, fmt.Errorf("write PyPI link-signing key: %w", err)
	}
	return secret, nil
}

// Serve handles /pypi/{upstream}/simple/{project}/ and signed artifact URLs.
func (h *Handler) Serve(w http.ResponseWriter, r *http.Request, upstreamName, path string) {
	upstream, ok := h.upstreams[upstreamName]
	if !ok || upstream.Kind != "pypi" {
		http.NotFound(w, r)
		return
	}
	if strings.HasPrefix(path, "simple/") {
		h.serveSimple(w, r, upstreamName, upstream, path)
		return
	}
	if strings.HasPrefix(path, "files/") {
		h.serveArtifact(w, r, upstreamName, path)
		return
	}
	http.NotFound(w, r)
}

func (h *Handler) serveSimple(w http.ResponseWriter, _ *http.Request, upstreamName string, upstream config.Upstream, path string) {
	if !validSimplePath(path) {
		http.NotFound(w, nil)
		return
	}
	response, err := h.fetchSimple(upstream, path)
	if err != nil {
		http.Error(w, "PyPI upstream unavailable", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()

	document, err := html.Parse(response.Body)
	if err != nil {
		http.Error(w, "invalid PyPI Simple response", http.StatusBadGateway)
		return
	}
	project := strings.TrimSuffix(strings.TrimPrefix(path, "simple/"), "/")
	h.rewriteLinks(document, response.Request.URL, upstreamName, project)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if serial := response.Header.Get("X-PyPI-Last-Serial"); serial != "" {
		w.Header().Set("X-PyPI-Last-Serial", serial)
	}
	if err := html.Render(w, document); err != nil {
		return
	}
}

func (h *Handler) serveArtifact(w http.ResponseWriter, r *http.Request, upstreamName, path string) {
	filename := strings.TrimPrefix(path, "files/")
	if filename == "" || strings.Contains(filename, "/") {
		http.NotFound(w, r)
		return
	}
	token := r.URL.Query().Get("token")
	project, sourceURL, ok := h.verify(token)
	if !ok {
		http.NotFound(w, r)
		return
	}
	h.cache.ServeURL(w, r, upstreamName, sourceURL, sourceURL, cache.ArtifactPolicy)
	if h.cache.Cached(upstreamName, sourceURL, cache.ArtifactPolicy) {
		_ = h.cache.WatchPackage("pypi", upstreamName, project)
	}
}

func (h *Handler) fetchSimple(upstream config.Upstream, path string) (*http.Response, error) {
	urls := append([]string{upstream.Primary}, upstream.Backups...)
	var lastErr error
	for _, base := range urls {
		target, err := joinURL(base, path)
		if err != nil {
			return nil, err
		}
		request, err := http.NewRequest(http.MethodGet, target, nil)
		if err != nil {
			return nil, err
		}
		// Requesting HTML keeps the link-rewriting behavior consistent even
		// when a modern pip client advertises the JSON Simple API variant.
		request.Header.Set("Accept", "text/html")
		response, err := h.client.Do(request)
		if err != nil {
			lastErr = err
			continue
		}
		if response.StatusCode >= 500 {
			lastErr = fmt.Errorf("%s returned %s", target, response.Status)
			response.Body.Close()
			continue
		}
		if response.StatusCode != http.StatusOK {
			response.Body.Close()
			return nil, fmt.Errorf("%s returned %s", target, response.Status)
		}
		return response, nil
	}
	return nil, fmt.Errorf("all PyPI upstreams failed: %w", lastErr)
}

func (h *Handler) rewriteLinks(node *html.Node, base *url.URL, upstreamName, project string) {
	if node.Type == html.ElementNode && node.Data == "a" {
		// PEP 658 sidecar metadata URLs are derived by pip from the artifact
		// URL. Signed query URLs are intentionally opaque, so omit the optional
		// hint and let pip retrieve the wheel/sdist itself.
		attributes := node.Attr[:0]
		for _, attribute := range node.Attr {
			if attribute.Key == "data-dist-info-metadata" || attribute.Key == "data-core-metadata" {
				continue
			}
			attributes = append(attributes, attribute)
		}
		node.Attr = attributes
		for index := range node.Attr {
			if node.Attr[index].Key != "href" {
				continue
			}
			target, err := base.Parse(node.Attr[index].Val)
			if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
				continue
			}
			fragment := target.Fragment
			target.Fragment = ""
			filename := pathBase(target.Path)
			if filename == "" {
				continue
			}
			node.Attr[index].Val = "/pypi/" + upstreamName + "/files/" + filename + "?token=" + h.sign(project, target.String())
			if fragment != "" {
				node.Attr[index].Val += "#" + fragment
			}
		}
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		h.rewriteLinks(child, base, upstreamName, project)
	}
}

func pathBase(value string) string {
	trimmed := strings.TrimSuffix(value, "/")
	if trimmed == "" {
		return ""
	}
	parts := strings.Split(trimmed, "/")
	return parts[len(parts)-1]
}

func (h *Handler) sign(project, sourceURL string) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(project + "\n" + sourceURL))
	mac := hmac.New(sha256.New, h.secret)
	_, _ = mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (h *Handler) verify(token string) (string, string, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	provided, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", "", false
	}
	mac := hmac.New(sha256.New, h.secret)
	_, _ = mac.Write([]byte(parts[0]))
	if !hmac.Equal(provided, mac.Sum(nil)) {
		return "", "", false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", "", false
	}
	parts = strings.SplitN(string(decoded), "\n", 2)
	if len(parts) != 2 || !validProject(parts[0]) {
		return "", "", false
	}
	project, sourceURL := parts[0], parts[1]
	parsed, err := url.Parse(sourceURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.Fragment != "" {
		return "", "", false
	}
	return project, sourceURL, true
}

func validSimplePath(path string) bool {
	parts := strings.Split(strings.TrimSuffix(path, "/"), "/")
	return len(parts) == 2 && parts[0] == "simple" && parts[1] != "" && parts[1] != "." && parts[1] != ".."
}

func validProject(project string) bool {
	return project != "" && !strings.ContainsAny(project, "/\\\n\r") && project != "." && project != ".."
}

type projectResponse struct {
	Releases map[string][]releaseFile `json:"releases"`
}

type releaseFile struct {
	Filename          string `json:"filename"`
	URL               string `json:"url"`
	PackageType       string `json:"packagetype"`
	Yanked            bool   `json:"yanked"`
	UploadTimeISO8601 string `json:"upload_time_iso_8601"`
}

type release struct {
	Version string
	Time    time.Time
	Files   []releaseFile
}

// StartPrefetch discovers a compact set of recent releases for PyPI packages
// that clients actually fetched. It intentionally chooses one broadly useful
// distribution per release (universal wheel, then sdist) instead of fetching
// every platform wheel published for a package.
func (h *Handler) StartPrefetch(ctx context.Context, interval, retention time.Duration, versions int) {
	go func() {
		_, _ = h.RefreshPackageWatches(interval, retention, versions)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, _ = h.RefreshPackageWatches(interval, retention, versions)
			}
		}
	}()
}

type PrefetchResult struct {
	Packages  int
	Artifacts int
	Expired   int64
}

func (h *Handler) RefreshPackageWatches(interval, retention time.Duration, versions int) (PrefetchResult, error) {
	if interval <= 0 || retention <= 0 || versions < 1 {
		return PrefetchResult{}, fmt.Errorf("prefetch interval, retention, and versions must be positive")
	}
	now := time.Now()
	result := PrefetchResult{}
	var err error
	result.Expired, err = h.store.DeleteInactivePackageWatches(now.Add(-retention))
	if err != nil {
		return result, err
	}
	watches, err := h.store.PackageWatchesDue("pypi", now.Add(-interval), now.Add(-retention))
	if err != nil {
		return result, err
	}
	for _, watch := range watches {
		upstream, ok := h.upstreams[watch.Upstream]
		if !ok || upstream.Kind != "pypi" {
			continue
		}
		response, err := h.fetchProject(upstream, watch.Package)
		if err != nil {
			continue
		}
		files := selectReleaseFiles(response.Releases, versions)
		for _, file := range files {
			if err := h.cache.PrefetchURL(watch.Upstream, file.URL); err == nil {
				result.Artifacts++
			}
		}
		if err := h.store.CheckedPackageWatch("pypi", watch.Upstream, watch.Package); err != nil {
			return result, err
		}
		result.Packages++
	}
	return result, nil
}

func (h *Handler) fetchProject(upstream config.Upstream, project string) (projectResponse, error) {
	urls := append([]string{upstream.Primary}, upstream.Backups...)
	var lastErr error
	for _, base := range urls {
		target, err := projectJSONURL(base, project)
		if err != nil {
			return projectResponse{}, err
		}
		response, err := h.client.Get(target)
		if err != nil {
			lastErr = err
			continue
		}
		if response.StatusCode >= 500 {
			lastErr = fmt.Errorf("%s returned %s", target, response.Status)
			response.Body.Close()
			continue
		}
		if response.StatusCode != http.StatusOK {
			response.Body.Close()
			return projectResponse{}, fmt.Errorf("%s returned %s", target, response.Status)
		}
		var decoded projectResponse
		err = json.NewDecoder(response.Body).Decode(&decoded)
		response.Body.Close()
		if err != nil {
			return projectResponse{}, fmt.Errorf("decode %s: %w", target, err)
		}
		return decoded, nil
	}
	return projectResponse{}, fmt.Errorf("all upstreams failed: %w", lastErr)
}

func projectJSONURL(base, project string) (string, error) {
	parsed, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/") + "/pypi/" + project + "/json"
	return parsed.String(), nil
}

func selectReleaseFiles(releases map[string][]releaseFile, count int) []releaseFile {
	available := make([]release, 0, len(releases))
	for version, files := range releases {
		var latest time.Time
		usable := make([]releaseFile, 0, len(files))
		for _, file := range files {
			if file.Yanked || file.URL == "" {
				continue
			}
			if timestamp, err := time.Parse(time.RFC3339Nano, file.UploadTimeISO8601); err == nil && timestamp.After(latest) {
				latest = timestamp
			}
			usable = append(usable, file)
		}
		if len(usable) != 0 {
			available = append(available, release{Version: version, Time: latest, Files: usable})
		}
	}
	sort.Slice(available, func(i, j int) bool {
		if available[i].Time.Equal(available[j].Time) {
			return available[i].Version > available[j].Version
		}
		return available[i].Time.After(available[j].Time)
	})
	if len(available) > count {
		available = available[:count]
	}
	result := make([]releaseFile, 0, len(available))
	for _, release := range available {
		result = append(result, preferredFile(release.Files))
	}
	return result
}

func preferredFile(files []releaseFile) releaseFile {
	for _, file := range files {
		if file.PackageType == "bdist_wheel" && (strings.HasSuffix(file.Filename, "-py3-none-any.whl") || strings.HasSuffix(file.Filename, "-py2.py3-none-any.whl")) {
			return file
		}
	}
	for _, file := range files {
		if file.PackageType == "sdist" {
			return file
		}
	}
	return files[0]
}

func joinURL(base, path string) (string, error) {
	parsed, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/") + "/" + path
	return parsed.String(), nil
}

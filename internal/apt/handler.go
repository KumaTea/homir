// Package apt provides the Debian repository-facing portion of Homir.
package apt

import (
	"bufio"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/ulikunitz/xz"

	"github.com/KumaTea/homir/internal/cache"
	"github.com/KumaTea/homir/internal/config"
	"github.com/KumaTea/homir/internal/store"
)

type Handler struct {
	cache     *cache.Manager
	store     *store.Store
	upstreams map[string]config.Upstream
}

func NewHandler(manager *cache.Manager, db *store.Store, upstreams map[string]config.Upstream) *Handler {
	return &Handler{cache: manager, store: db, upstreams: upstreams}
}

// Serve handles paths below /apt/{configured-upstream}/. It deliberately
// preserves dists/ metadata bytes, including APT signatures, instead of
// generating a replacement repository.
func (h *Handler) Serve(w http.ResponseWriter, r *http.Request, upstreamName, repositoryPath string) {
	upstream, ok := h.upstreams[upstreamName]
	if !ok || upstream.Kind != "apt" {
		http.NotFound(w, r)
		return
	}

	if isArtifact(repositoryPath) {
		h.cache.Serve(w, r, upstreamName, repositoryPath)
		if h.cache.Cached(upstreamName, repositoryPath, cache.ArtifactPolicy) {
			if record, found, err := h.store.APTRecordForArtifact(upstreamName, repositoryPath); err == nil && found {
				_ = h.store.WatchPackageVariant("apt", upstreamName, record.Package, record.Architecture)
			}
		}
		return
	}
	ttl, err := upstream.MetadataRefreshInterval()
	if err != nil {
		http.Error(w, "invalid upstream metadata policy", http.StatusInternalServerError)
		return
	}
	h.cache.ServeWithPolicy(w, r, upstreamName, repositoryPath, cache.Policy{
		Namespace:    "apt-metadata",
		RefreshAfter: ttl,
	})
	if isPackagesIndex(repositoryPath) {
		h.catalogIndex(upstreamName, repositoryPath)
	}
}

func isArtifact(path string) bool {
	return strings.HasSuffix(path, ".deb") || strings.HasSuffix(path, ".udeb") || strings.HasSuffix(path, ".ddeb")
}

func isPackagesIndex(repositoryPath string) bool {
	name := path.Base(repositoryPath)
	return strings.Contains(repositoryPath, "/binary-") && (name == "Packages" || name == "Packages.gz" || name == "Packages.xz")
}

func (h *Handler) catalogIndex(upstreamName, indexPath string) {
	file, err := h.cache.OpenCached(upstreamName, indexPath, cache.Policy{Namespace: "apt-metadata"})
	if err != nil {
		return
	}
	defer file.Close()
	var reader io.Reader = file
	switch {
	case strings.HasSuffix(indexPath, ".gz"):
		gzipReader, err := gzip.NewReader(file)
		if err != nil {
			return
		}
		defer gzipReader.Close()
		reader = gzipReader
	case strings.HasSuffix(indexPath, ".xz"):
		xzReader, err := xz.NewReader(file)
		if err != nil {
			return
		}
		reader = xzReader
	}
	records, err := parsePackages(reader)
	if err == nil {
		_ = h.store.ReplaceAPTCatalog(upstreamName, indexPath, records)
	}
}

func parsePackages(reader io.Reader) ([]store.APTRecord, error) {
	scanner := bufio.NewScanner(reader)
	buffer := make([]byte, 64*1024)
	scanner.Buffer(buffer, 4*1024*1024)
	var records []store.APTRecord
	fields := make(map[string]string)
	flush := func() {
		if fields["Package"] != "" && fields["Version"] != "" && fields["Filename"] != "" {
			records = append(records, store.APTRecord{Package: fields["Package"], Version: fields["Version"], Architecture: fields["Architecture"], ArtifactPath: fields["Filename"]})
		}
		fields = make(map[string]string)
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 {
			fields[parts[0]] = strings.TrimSpace(parts[1])
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	flush()
	return records, nil
}

type PrefetchResult struct {
	Packages  int
	Artifacts int
	Expired   int64
}

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
	watches, err := h.store.PackageWatchesDue("apt", now.Add(-interval), now.Add(-retention))
	if err != nil {
		return result, err
	}
	for _, watch := range watches {
		records, err := h.store.APTRecords(watch.Upstream, watch.Package)
		if err != nil {
			continue
		}
		for _, record := range selectRecords(records, watch.Variant, versions) {
			if err := h.cache.Prefetch(watch.Upstream, record.ArtifactPath); err == nil {
				result.Artifacts++
			}
		}
		if err := h.store.CheckedPackageWatch("apt", watch.Upstream, watch.Package); err != nil {
			return result, err
		}
		result.Packages++
	}
	return result, nil
}

func selectRecords(records []store.APTRecord, architecture string, limit int) []store.APTRecord {
	byVersion := make(map[string]store.APTRecord)
	for _, record := range records {
		if architecture != "" && record.Architecture != architecture && record.Architecture != "all" {
			continue
		}
		prior, found := byVersion[record.Version]
		if !found || (prior.Architecture == "all" && record.Architecture == architecture) {
			byVersion[record.Version] = record
		}
	}
	selected := make([]store.APTRecord, 0, len(byVersion))
	for _, record := range byVersion {
		selected = append(selected, record)
	}
	sort.Slice(selected, func(i, j int) bool { return debianVersionCompare(selected[i].Version, selected[j].Version) > 0 })
	if len(selected) > limit {
		selected = selected[:limit]
	}
	return selected
}

// Package apk provides Alpine Package Keeper repository routing for Homir.
package apk

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

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

// Serve handles paths below /apk/{configured-upstream}/. Alpine verifies the
// signed upstream index itself, so Homir relays that index without alteration.
func (h *Handler) Serve(w http.ResponseWriter, r *http.Request, upstreamName, repositoryPath string) {
	upstream, ok := h.upstreams[upstreamName]
	if !ok || upstream.Kind != "apk" {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(repositoryPath, ".apk") {
		h.cache.Serve(w, r, upstreamName, repositoryPath)
		if h.cache.Cached(upstreamName, repositoryPath, cache.ArtifactPolicy) {
			if record, found, err := h.store.APKRecordForArtifact(upstreamName, repositoryPath); err == nil && found {
				_ = h.store.WatchPackageVariant("apk", upstreamName, record.Package, record.Architecture)
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
		Namespace:    "apk-metadata",
		RefreshAfter: ttl,
	})
	if repositoryPath == "APKINDEX.tar.gz" {
		h.catalogIndex(upstreamName, repositoryPath)
	}
}

func (h *Handler) catalogIndex(upstreamName, indexPath string) {
	file, err := h.cache.OpenCached(upstreamName, indexPath, cache.Policy{Namespace: "apk-metadata"})
	if err != nil {
		return
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			return
		}
		if err != nil {
			return
		}
		if header.Name != "APKINDEX" {
			continue
		}
		records, err := parseIndex(tarReader)
		if err == nil {
			_ = h.store.ReplaceAPKCatalog(upstreamName, indexPath, records)
		}
		return
	}
}

func parseIndex(reader io.Reader) ([]store.APKRecord, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var records []store.APKRecord
	fields := make(map[string]string)
	flush := func() {
		if fields["P"] != "" && fields["V"] != "" {
			records = append(records, store.APKRecord{Package: fields["P"], Version: fields["V"], Architecture: fields["A"], ArtifactPath: fields["P"] + "-" + fields["V"] + ".apk"})
		}
		fields = make(map[string]string)
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			flush()
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 {
			fields[parts[0]] = parts[1]
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
	watches, err := h.store.PackageWatchesDue("apk", now.Add(-interval), now.Add(-retention))
	if err != nil {
		return result, err
	}
	for _, watch := range watches {
		records, err := h.store.APKRecords(watch.Upstream, watch.Package)
		if err != nil {
			continue
		}
		for _, record := range selectRecords(records, watch.Variant, versions) {
			if err := h.cache.Prefetch(watch.Upstream, record.ArtifactPath); err == nil {
				result.Artifacts++
			}
		}
		if err := h.store.CheckedPackageWatch("apk", watch.Upstream, watch.Package); err != nil {
			return result, err
		}
		result.Packages++
	}
	return result, nil
}

func selectRecords(records []store.APKRecord, architecture string, limit int) []store.APKRecord {
	byVersion := make(map[string]store.APKRecord)
	for _, record := range records {
		if architecture != "" && record.Architecture != architecture && record.Architecture != "noarch" {
			continue
		}
		prior, found := byVersion[record.Version]
		if !found || (prior.Architecture == "noarch" && record.Architecture == architecture) {
			byVersion[record.Version] = record
		}
	}
	selected := make([]store.APKRecord, 0, len(byVersion))
	for _, record := range byVersion {
		selected = append(selected, record)
	}
	sort.Slice(selected, func(i, j int) bool { return apkVersionCompare(selected[i].Version, selected[j].Version) > 0 })
	if len(selected) > limit {
		selected = selected[:limit]
	}
	return selected
}

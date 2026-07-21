// Package apt provides the Debian repository-facing portion of Homir.
package apt

import (
	"bufio"
	"compress/gzip"
	"io"
	"net/http"
	"path"
	"strings"

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
				_ = h.cache.WatchPackage("apt", upstreamName, record.Package)
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
			records = append(records, store.APTRecord{Package: fields["Package"], Version: fields["Version"], ArtifactPath: fields["Filename"]})
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

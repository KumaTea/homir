// Package apt provides the Debian repository-facing portion of Homir.
package apt

import (
	"net/http"
	"strings"

	"github.com/KumaTea/homir/internal/cache"
	"github.com/KumaTea/homir/internal/config"
)

type Handler struct {
	cache     *cache.Manager
	upstreams map[string]config.Upstream
}

func NewHandler(manager *cache.Manager, upstreams map[string]config.Upstream) *Handler {
	return &Handler{cache: manager, upstreams: upstreams}
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
}

func isArtifact(path string) bool {
	return strings.HasSuffix(path, ".deb") || strings.HasSuffix(path, ".udeb") || strings.HasSuffix(path, ".ddeb")
}

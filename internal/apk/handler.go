// Package apk provides Alpine Package Keeper repository routing for Homir.
package apk

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
}

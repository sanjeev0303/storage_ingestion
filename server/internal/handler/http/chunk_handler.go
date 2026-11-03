package http

import (
	nethttp "net/http"
	"strconv"
)

// ChunkRoutes serve the chunk index, the savings report and version deltas
// (docs/chunks.md).
func (h *DatasetHandler) ChunkRoutes() map[string]nethttp.HandlerFunc {
	const version = "/api/v1/datasets/{id}/versions/{versionID}"
	return map[string]nethttp.HandlerFunc{
		"GET /api/v1/datasets/{id}/chunk-savings":            h.chunkSavings,
		"PUT /api/v1/datasets/{id}/chunking":                 h.setChunking,
		"GET /api/v1/datasets/{id}/chunk-storage":            h.chunkStorage,
		"PUT /api/v1/datasets/{id}/chunk-storage":            h.setChunkStorage,
		"POST /api/v1/datasets/{id}/chunk-storage/rehydrate": h.rehydrateDataset,
		"POST " + version + "/chunks/index":                  h.indexChunks,
		"GET " + version + "/chunks/status":                  h.chunkStatus,
		"GET " + version + "/chunks":                         h.chunkManifest,
		"GET " + version + "/delta":                          h.chunkDelta,
	}
}

func (h *DatasetHandler) chunkSavings(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.ChunkSavings(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *DatasetHandler) setChunking(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := h.service.SetChunking(r.Context(), principalOf(r), r.PathValue("id"), body.Enabled); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]bool{"enabled": body.Enabled})
}

func (h *DatasetHandler) chunkStorage(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.ChunkStorageStats(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *DatasetHandler) setChunkStorage(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := h.service.SetChunkStorage(r.Context(), principalOf(r), r.PathValue("id"), body.Enabled); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]bool{"enabled": body.Enabled})
}

func (h *DatasetHandler) rehydrateDataset(w nethttp.ResponseWriter, r *nethttp.Request) {
	n, err := h.service.RehydrateDataset(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusAccepted, map[string]int64{"files": n})
}

func (h *DatasetHandler) indexChunks(w nethttp.ResponseWriter, r *nethttp.Request) {
	if err := h.service.IndexVersion(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("versionID")); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusAccepted, map[string]string{"state": "PENDING"})
}

func (h *DatasetHandler) chunkStatus(w nethttp.ResponseWriter, r *nethttp.Request) {
	job, found, err := h.service.ChunkJobOf(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("versionID"))
	if err != nil {
		h.fail(w, err)
		return
	}
	if !found {
		writeJSON(w, nethttp.StatusOK, map[string]any{"state": "NONE", "version_id": r.PathValue("versionID"), "skipped": []string{}})
		return
	}
	writeJSON(w, nethttp.StatusOK, job)
}

func (h *DatasetHandler) chunkManifest(w nethttp.ResponseWriter, r *nethttp.Request) {
	q := r.URL.Query()
	after, _ := strconv.Atoi(q.Get("after"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	sha, chunks, indexed, err := h.service.ChunkManifest(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("versionID"), q.Get("path"), after, limit)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"sha256": sha, "indexed": indexed, "chunks": chunks})
}

func (h *DatasetHandler) chunkDelta(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.ChunkDelta(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("versionID"), r.URL.Query().Get("from"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}

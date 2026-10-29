package http

import (
	nethttp "net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/dataset"
)

// Training runs: which dataset versions a model was trained on.

type modelBody struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	URI     string `json:"uri"`
}

func (b *modelBody) input() *dataset.ModelInput {
	if b == nil {
		return nil
	}
	return &dataset.ModelInput{Name: b.Name, Version: b.Version, URI: b.URI}
}

type modelResponse struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Version   string    `json:"version"`
	URI       string    `json:"uri,omitempty"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
}

type trainingRunResponse struct {
	ID         string             `json:"id"`
	Name       string             `json:"name"`
	Source     string             `json:"source"`
	ExternalID string             `json:"external_id,omitempty"`
	URL        string             `json:"url,omitempty"`
	Status     string             `json:"status"`
	Params     map[string]string  `json:"params"`
	Metrics    map[string]float64 `json:"metrics"`
	VersionIDs []string           `json:"dataset_version_ids"`
	Models     []modelResponse    `json:"models"`
	CreatedBy  string             `json:"created_by"`
	CreatedAt  time.Time          `json:"created_at"`
	UpdatedAt  time.Time          `json:"updated_at"`
	FinishedAt *time.Time         `json:"finished_at,omitempty"`
}

func trainingRunResponseOf(run model.TrainingRun) trainingRunResponse {
	out := trainingRunResponse{
		ID: run.ID, Name: run.Name, Source: string(run.Source), ExternalID: run.ExternalID, URL: run.URL, Status: string(run.Status),
		Params: run.Params, Metrics: run.Metrics, VersionIDs: run.VersionIDs, Models: []modelResponse{},
		CreatedBy: run.CreatedBy, CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt, FinishedAt: run.FinishedAt,
	}
	for _, m := range run.Models {
		out.Models = append(out.Models, modelResponse{ID: m.ID, Name: m.Name, Version: m.Version, URI: m.URI, CreatedBy: m.CreatedBy, CreatedAt: m.CreatedAt})
	}
	return out
}

func (h *DatasetHandler) recordTrainingRun(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		Name       string             `json:"name"`
		Source     string             `json:"source"`
		ExternalID string             `json:"external_id"`
		URL        string             `json:"url"`
		Status     string             `json:"status"`
		Params     map[string]string  `json:"params"`
		Metrics    map[string]float64 `json:"metrics"`
		VersionIDs []string           `json:"dataset_version_ids"`
		Model      *modelBody         `json:"model"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	run, created, err := h.service.RecordTrainingRun(r.Context(), principalOf(r), dataset.TrainingRunInput{
		Name: body.Name, Source: model.TrainingSource(strings.ToUpper(body.Source)), ExternalID: body.ExternalID, URL: body.URL,
		Status: model.TrainingStatus(strings.ToUpper(body.Status)), Params: body.Params, Metrics: body.Metrics,
		VersionIDs: body.VersionIDs, Model: body.Model.input(),
	})
	if err != nil {
		h.fail(w, err)
		return
	}
	status := nethttp.StatusOK
	if created {
		status = nethttp.StatusCreated
	}
	writeJSON(w, status, trainingRunResponseOf(run))
}

func (h *DatasetHandler) updateTrainingRun(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		Status  string             `json:"status"`
		Metrics map[string]float64 `json:"metrics"`
		Model   *modelBody         `json:"model"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	run, err := h.service.UpdateTrainingRun(r.Context(), principalOf(r), r.PathValue("runID"), dataset.UpdateTrainingRunInput{
		Status: model.TrainingStatus(strings.ToUpper(body.Status)), Metrics: body.Metrics, Model: body.Model.input(),
	})
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, trainingRunResponseOf(run))
}

func (h *DatasetHandler) trainingRun(w nethttp.ResponseWriter, r *nethttp.Request) {
	run, err := h.service.TrainingRun(r.Context(), principalOf(r), r.PathValue("runID"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, trainingRunResponseOf(run))
}

func (h *DatasetHandler) trainingRuns(w nethttp.ResponseWriter, r *nethttp.Request) {
	q := r.URL.Query()
	limit := 0
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			h.fail(w, dataset.ErrInvalidRequest)
			return
		}
		limit = n
	}
	page, err := h.service.TrainingRuns(r.Context(), principalOf(r), q.Get("dataset_version_id"), q.Get("cursor"), limit)
	if err != nil {
		h.fail(w, err)
		return
	}
	runs := make([]trainingRunResponse, 0, len(page.Runs))
	for _, run := range page.Runs {
		runs = append(runs, trainingRunResponseOf(run))
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"runs": runs, "next_cursor": page.NextCursor})
}

func (h *DatasetHandler) trainingRunLineage(w nethttp.ResponseWriter, r *nethttp.Request) {
	req, err := lineageRequest(r)
	if err != nil {
		h.fail(w, err)
		return
	}
	id := r.PathValue("runID")
	page, err := h.service.TrainingRunLineage(r.Context(), principalOf(r), id, req)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, lineageResponse(model.LineageRef{Kind: model.NodeTrainingRun, RefID: id}, req.Direction, page))
}

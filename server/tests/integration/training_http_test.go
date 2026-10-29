package integration

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/response"
)

type runAnswer struct {
	ID         string             `json:"id"`
	Name       string             `json:"name"`
	Source     string             `json:"source"`
	Status     string             `json:"status"`
	Metrics    map[string]float64 `json:"metrics"`
	VersionIDs []string           `json:"dataset_version_ids"`
	Models     []struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"models"`
	FinishedAt *string `json:"finished_at"`
}

// A training run links the dataset versions it read to the model it
// produced, and lineage follows the link both ways.
func TestHTTPTrainingRunLineage(t *testing.T) {
	s := openSecurityStack(t, nil)
	acme := s.team(t, "train-a")
	globex := s.team(t, "train-b")
	tok := s.signIn(t, acme.devUser.Email, acme.devPW).AccessToken
	globexTok := s.signIn(t, globex.devUser.Email, globex.devPW).AccessToken
	objs := s.publishObjects(t, acme.developer, "tr", 2)

	var ds response.DatasetResponse
	s.expect(t, tok, "", http.MethodPost, "/api/v1/datasets", `{"name":"tr-set"}`, http.StatusCreated, &ds)
	dsPath := "/api/v1/datasets/" + ds.ID
	s.expect(t, tok, "", http.MethodPost, dsPath+"/members", fmt.Sprintf(`{"members":[{"object_id":%q},{"object_id":%q}]}`,
		objs[0].ObjectID, objs[1].ObjectID), http.StatusOK, nil)
	var v1 response.DatasetVersionResponse
	s.expect(t, tok, "", http.MethodPost, dsPath+"/snapshots", `{"idempotency_key":"tr-1"}`, http.StatusCreated, &v1)

	body := fmt.Sprintf(`{"name":"resnet-run","source":"mlflow","external_id":"mlflow-run-42","url":"https://mlflow.example.com/#/runs/42",
		"params":{"lr":"0.01"},"dataset_version_ids":[%q]}`, v1.ID)
	var run runAnswer
	s.expect(t, tok, "", http.MethodPost, "/api/v1/training-runs", body, http.StatusCreated, &run)
	if run.Source != "MLFLOW" || run.Status != "RUNNING" || len(run.VersionIDs) != 1 || run.VersionIDs[0] != v1.ID {
		t.Fatalf("recorded run: %+v", run)
	}
	// The same external run again is the same record.
	var again runAnswer
	s.expect(t, tok, "", http.MethodPost, "/api/v1/training-runs", body, http.StatusOK, &again)
	if again.ID != run.ID {
		t.Fatalf("the run was recorded twice: %s and %s", run.ID, again.ID)
	}

	// Finishing it records metrics and the model.
	runPath := "/api/v1/training-runs/" + run.ID
	s.expect(t, tok, "", http.MethodPatch, runPath,
		`{"status":"finished","metrics":{"accuracy":0.91},"model":{"name":"resnet","version":"1","uri":"s3://models/resnet/1"}}`, http.StatusOK, &run)
	if run.Status != "FINISHED" || run.FinishedAt == nil || run.Metrics["accuracy"] != 0.91 || len(run.Models) != 1 {
		t.Fatalf("finished run: %+v", run)
	}
	if r := s.call(t, tok, "", http.MethodPatch, runPath, `{"model":{"name":"resnet","version":"1"}}`); r.status != http.StatusConflict {
		t.Fatalf("a model name and version twice: %s", r)
	}

	// Downstream from the version: the run, then its model.
	var down response.LineageResponse
	s.expect(t, tok, "", http.MethodGet, "/api/v1/dataset-versions/"+v1.ID+"/lineage?direction=downstream", "", http.StatusOK, &down)
	relations := map[string]bool{}
	for _, e := range down.Edges {
		relations[e.Relation+":"+e.From.Kind] = true
	}
	if !relations["TRAINED_ON:TRAINING_RUN"] || !relations["PRODUCED_BY:MODEL"] {
		t.Fatalf("downstream of the version: %+v", down.Edges)
	}
	// Upstream from the run: the version it trained on.
	var up response.LineageResponse
	s.expect(t, tok, "", http.MethodGet, runPath+"/lineage", "", http.StatusOK, &up)
	if len(up.Edges) == 0 || up.Edges[0].Relation != "TRAINED_ON" || up.Edges[0].To.ID != v1.ID {
		t.Fatalf("upstream of the run: %+v", up.Edges)
	}

	// Listed by the version it used.
	var list struct {
		Runs []runAnswer `json:"runs"`
	}
	s.expect(t, tok, "", http.MethodGet, "/api/v1/training-runs?dataset_version_id="+v1.ID, "", http.StatusOK, &list)
	if len(list.Runs) != 1 || list.Runs[0].ID != run.ID {
		t.Fatalf("runs of the version: %+v", list.Runs)
	}

	// Refusals: bad input, unknown versions, and other organizations.
	for name, bad := range map[string]string{
		"no versions": `{"name":"x","dataset_version_ids":[]}`,
		"bad source":  fmt.Sprintf(`{"name":"x","source":"jupyter","dataset_version_ids":[%q]}`, v1.ID),
		"script url":  fmt.Sprintf(`{"name":"x","url":"javascript:alert(1)","dataset_version_ids":[%q]}`, v1.ID),
		"empty name":  fmt.Sprintf(`{"name":" ","dataset_version_ids":[%q]}`, v1.ID),
		"infinite":    fmt.Sprintf(`{"name":"x","metrics":{"loss":1e999},"dataset_version_ids":[%q]}`, v1.ID),
	} {
		if r := s.call(t, tok, "", http.MethodPost, "/api/v1/training-runs", bad); r.status != http.StatusBadRequest {
			t.Fatalf("%s: %s", name, r)
		}
	}
	if r := s.call(t, tok, "", http.MethodPost, "/api/v1/training-runs", `{"name":"x","dataset_version_ids":["nope"]}`); r.status != http.StatusNotFound {
		t.Fatalf("an unknown version: %s", r)
	}
	if r := s.call(t, globexTok, "", http.MethodPost, "/api/v1/training-runs",
		fmt.Sprintf(`{"name":"theirs","dataset_version_ids":[%q]}`, v1.ID)); r.status != http.StatusNotFound {
		t.Fatalf("another organization's version: %s", r)
	}
	if r := s.call(t, globexTok, "", http.MethodGet, runPath, ""); r.status != http.StatusNotFound {
		t.Fatalf("another organization's run: %s", r)
	}
}

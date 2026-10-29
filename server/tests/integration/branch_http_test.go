package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/response"
)

type mergeAnswer struct {
	ID           string `json:"id"`
	Applied      bool   `json:"applied"`
	Added        int64  `json:"added"`
	Updated      int64  `json:"updated"`
	Removed      int64  `json:"removed"`
	AlreadySame  int64  `json:"already_same"`
	Conflicts    int64  `json:"conflicts"`
	Code         string `json:"code"`
	ConflictList []struct {
		ObjectID string `json:"object_id"`
		Base     struct {
			Label string `json:"label"`
		} `json:"base"`
		Source struct {
			Label string `json:"label"`
		} `json:"source"`
		Target struct {
			Label string `json:"label"`
		} `json:"target"`
	} `json:"conflict_list"`
}

func TestHTTPDatasetBranchAndMerge(t *testing.T) {
	s := openSecurityStack(t, nil)
	acme := s.team(t, "branch-a")
	globex := s.team(t, "branch-b")
	tok := s.signIn(t, acme.devUser.Email, acme.devPW).AccessToken
	globexTok := s.signIn(t, globex.devUser.Email, globex.devPW).AccessToken
	objs := s.publishObjects(t, acme.developer, "br", 5)

	var main response.DatasetResponse
	s.expect(t, tok, "", http.MethodPost, "/api/v1/datasets", `{"name":"br-set"}`, http.StatusCreated, &main)
	mainPath := "/api/v1/datasets/" + main.ID
	if r := s.call(t, tok, "", http.MethodPost, mainPath+"/branches", `{"name":"early"}`); r.status != http.StatusConflict {
		t.Fatalf("a branch before any snapshot: %s", r)
	}
	s.expect(t, tok, "", http.MethodPost, mainPath+"/members", fmt.Sprintf(`{"members":[
		{"object_id":%q,"label":"cat"},{"object_id":%q,"label":"dog"},{"object_id":%q},{"object_id":%q}]}`,
		objs[0].ObjectID, objs[1].ObjectID, objs[2].ObjectID, objs[3].ObjectID), http.StatusOK, nil)
	var v1 response.DatasetVersionResponse
	s.expect(t, tok, "", http.MethodPost, mainPath+"/snapshots", `{"idempotency_key":"br-1"}`, http.StatusCreated, &v1)

	// A branch starts with the snapshot's members.
	var branch response.DatasetResponse
	s.expect(t, tok, "", http.MethodPost, mainPath+"/branches", `{"name":"exp"}`, http.StatusCreated, &branch)
	if branch.Branch == nil || branch.Branch.ParentID != main.ID || branch.Branch.ForkVersionID != v1.ID || branch.MemberCount != 4 {
		t.Fatalf("branch: %+v %+v", branch, branch.Branch)
	}
	branchPath := "/api/v1/datasets/" + branch.ID
	for body, want := range map[string]int{`{"name":"exp"}`: http.StatusConflict, `{"name":"Not Valid!"}`: http.StatusBadRequest} {
		if r := s.call(t, tok, "", http.MethodPost, mainPath+"/branches", body); r.status != want {
			t.Fatalf("branch %s: %s, want %d", body, r, want)
		}
	}
	if r := s.call(t, tok, "", http.MethodPost, branchPath+"/branches", `{"name":"deeper"}`); r.status != http.StatusBadRequest {
		t.Fatalf("a branch of a branch: %s", r)
	}
	if r := s.call(t, globexTok, "", http.MethodPost, mainPath+"/branches", `{"name":"theirs"}`); r.status != http.StatusNotFound {
		t.Fatalf("another organization branches acme's dataset: %s", r)
	}
	var listed response.DatasetPageResponse
	s.expect(t, tok, "", http.MethodGet, "/api/v1/datasets", "", http.StatusOK, &listed)
	for _, d := range listed.Datasets {
		if d.ID == branch.ID {
			t.Fatal("a branch is listed among the datasets")
		}
	}
	var branches struct {
		Branches []response.DatasetResponse `json:"branches"`
	}
	s.expect(t, tok, "", http.MethodGet, mainPath+"/branches", "", http.StatusOK, &branches)
	if len(branches.Branches) != 1 || branches.Branches[0].Branch.Name != "exp" {
		t.Fatalf("branches: %+v", branches)
	}
	if r := s.call(t, tok, "", http.MethodPost, "/api/v1/ingestion/jobs", fmt.Sprintf(`{"dataset_id":%q,"idempotency_key":"b"}`, branch.ID)); r.status != http.StatusBadRequest {
		t.Fatalf("ingestion into a branch: %s", r)
	}

	members := func(path string) map[string]response.DatasetMemberResponse {
		t.Helper()
		var page response.DatasetMemberPageResponse
		s.expect(t, tok, "", http.MethodGet, path+"/members", "", http.StatusOK, &page)
		out := map[string]response.DatasetMemberResponse{}
		for _, m := range page.Members {
			out[m.ObjectID] = m
		}
		return out
	}

	// The branch relabels 0, removes 1 and adds 4, then commits.
	s.expect(t, tok, "", http.MethodPost, branchPath+"/members", fmt.Sprintf(`{"on_duplicate":"REPLACE","members":[
		{"object_id":%q,"label":"lion"},{"object_id":%q}]}`, objs[0].ObjectID, objs[4].ObjectID), http.StatusOK, nil)
	s.expect(t, tok, "", http.MethodDelete, branchPath+"/members/"+members(branchPath)[objs[1].ObjectID].ID, "", http.StatusOK, nil)
	var b1 response.DatasetVersionResponse
	s.expect(t, tok, "", http.MethodPost, branchPath+"/snapshots", `{"idempotency_key":"b-1"}`, http.StatusCreated, &b1)

	// Meanwhile the parent relabels 2 (no conflict) and 0 (a conflict).
	s.expect(t, tok, "", http.MethodPost, mainPath+"/members", fmt.Sprintf(`{"on_duplicate":"REPLACE","members":[
		{"object_id":%q,"label":"tiger"},{"object_id":%q,"label":"mouse"}]}`, objs[0].ObjectID, objs[2].ObjectID), http.StatusOK, nil)

	merge := func(path, body string, want int) mergeAnswer {
		t.Helper()
		var out mergeAnswer
		r := s.expect(t, tok, "", http.MethodPost, path+"/merges", body, want, nil)
		if err := json.Unmarshal(r.body, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	from := func(source, extra string) string {
		return fmt.Sprintf(`{"source_dataset_id":%q%s}`, source, extra)
	}

	dry := merge(mainPath, from(branch.ID, `,"dry_run":true`), http.StatusOK)
	if dry.Applied || dry.Added != 1 || dry.Removed != 1 || dry.Conflicts != 1 || dry.Updated != 0 {
		t.Fatalf("dry run: %+v", dry)
	}
	failed := merge(mainPath, from(branch.ID, ""), http.StatusConflict)
	c := failed.ConflictList
	if failed.Applied || failed.Code != "merge_conflict" || len(c) != 1 || c[0].ObjectID != objs[0].ObjectID ||
		c[0].Base.Label != "cat" || c[0].Source.Label != "lion" || c[0].Target.Label != "tiger" {
		t.Fatalf("conflicting merge: %+v", failed)
	}
	if got := members(mainPath); len(got) != 4 || got[objs[0].ObjectID].Label != "tiger" {
		t.Fatalf("a refused merge changed the draft: %v", got)
	}

	done := merge(mainPath, from(branch.ID, `,"strategy":"source","idempotency_key":"m-1"`), http.StatusOK)
	if !done.Applied || done.Added != 1 || done.Removed != 1 || done.Updated != 1 || done.Conflicts != 1 {
		t.Fatalf("merge taking the branch's side: %+v", done)
	}
	got := members(mainPath)
	if len(got) != 4 || got[objs[0].ObjectID].Label != "lion" || got[objs[2].ObjectID].Label != "mouse" {
		t.Fatalf("parent after the merge: %v", got)
	}
	if _, kept := got[objs[1].ObjectID]; kept {
		t.Fatal("the branch's removal was not merged")
	}
	if _, added := got[objs[4].ObjectID]; !added {
		t.Fatal("the branch's addition was not merged")
	}
	again := merge(mainPath, from(branch.ID, `,"strategy":"source","idempotency_key":"m-1"`), http.StatusOK)
	if again.ID != done.ID {
		t.Fatalf("a repeated key merged twice: %s and %s", again.ID, done.ID)
	}
	var history struct {
		Merges []mergeAnswer `json:"merges"`
	}
	s.expect(t, tok, "", http.MethodGet, mainPath+"/merges", "", http.StatusOK, &history)
	if len(history.Merges) != 1 {
		t.Fatalf("merge history: %+v", history)
	}
	s.expect(t, tok, "", http.MethodGet, branchPath, "", http.StatusOK, &branch)
	if branch.Branch.MergeBaseVersionID != b1.ID {
		t.Fatalf("merge base after the merge: %+v", branch.Branch)
	}

	// Diff across the family: the branch's commit against the parent's.
	var diff struct {
		Summary map[string]int64 `json:"summary"`
	}
	s.expect(t, tok, "", http.MethodGet, branchPath+"/versions/"+b1.ID+"/diff?against="+v1.ID, "", http.StatusOK, &diff)
	if diff.Summary["added"] != 1 || diff.Summary["removed"] != 1 || diff.Summary["relabeled"] != 1 {
		t.Fatalf("branch against parent: %+v", diff.Summary)
	}

	// The parent's newer work flows back into the branch: only the
	// relabel of 2, which the branch never touched.
	var v2 response.DatasetVersionResponse
	s.expect(t, tok, "", http.MethodPost, mainPath+"/snapshots", `{"idempotency_key":"br-2"}`, http.StatusCreated, &v2)
	back := merge(branchPath, from(main.ID, ""), http.StatusOK)
	if !back.Applied || back.Updated != 1 || back.Added != 0 || back.Removed != 0 || back.Conflicts != 0 {
		t.Fatalf("parent into branch: %+v", back)
	}
	if got := members(branchPath); got[objs[2].ObjectID].Label != "mouse" || got[objs[0].ObjectID].Label != "lion" {
		t.Fatalf("branch after taking the parent's changes: %v", got)
	}
	if r := s.call(t, globexTok, "", http.MethodPost, mainPath+"/merges", from(branch.ID, "")); r.status != http.StatusNotFound {
		t.Fatalf("another organization merges: %s", r)
	}
}

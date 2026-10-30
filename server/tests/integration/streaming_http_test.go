package integration

import (
	"archive/tar"
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/response"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/config"
)

// Shards and shard streams over HTTP: one tar per shard, in path order,
// each file with its SHA-256; a pack that changed underneath cuts the
// stream. Then signed manifests, packing settings, repacks, and a run's
// verification reports and checkpoints.
func TestHTTPShardStreamsAndRunVerification(t *testing.T) {
	s := openSecurityStack(t, nil)
	acme := s.team(t, "shards")
	owner := s.signIn(t, acme.ownerUser.Email, acme.ownerPW).AccessToken
	dev := s.signIn(t, acme.devUser.Email, acme.devPW).AccessToken

	files := s.publishObjects(t, acme.developer, "sh", 6)
	rig := s.packingRig(t)
	w := rig.worker(t, "w-shards", nil, []string{acme.org.ID}, func(c *config.Packing) { c.TargetPackBytes, c.MaxObjectBytes = 90, 50 })
	for i := 0; i < 3; i++ {
		rig.run(t, w)
	}
	loose := s.publishAt(t, acme.developer, "sh/zz-loose.txt", textData("loose", 70))
	files = append(files, loose)
	want := map[string][]byte{"sh/zz-loose.txt": textData("loose", 70)}
	for i := range 6 {
		want[fmt.Sprintf("sh/item-%02d.txt", i)] = textData(fmt.Sprintf("sh-%d", i), 40+i)
	}

	var ds response.DatasetResponse
	s.expect(t, dev, "", http.MethodPost, "/api/v1/datasets", `{"name":"shards-set"}`, http.StatusCreated, &ds)
	dsPath := "/api/v1/datasets/" + ds.ID
	var members []string
	for _, a := range files {
		members = append(members, fmt.Sprintf(`{"object_id":%q}`, a.ObjectID))
	}
	s.expect(t, dev, "", http.MethodPost, dsPath+"/members", `{"members":[`+strings.Join(members, ",")+`]}`, http.StatusOK, nil)
	var v response.DatasetVersionResponse
	s.expect(t, dev, "", http.MethodPost, dsPath+"/snapshots", `{"idempotency_key":"sh-1"}`, http.StatusCreated, &v)
	vPath := dsPath + "/versions/" + v.ID

	var page struct {
		Shards []model.Shard `json:"shards"`
	}
	s.expect(t, dev, "", http.MethodGet, vPath+"/shards?target_bytes=1048576", "", http.StatusOK, &page)
	if len(page.Shards) != 1 || page.Shards[0].Files != 7 || page.Shards[0].Packs != 3 {
		t.Fatalf("one shard of seven files from three packs: %+v", page.Shards)
	}
	s.expect(t, dev, "", http.MethodGet, vPath+"/shards?target_bytes=10", "", http.StatusBadRequest, nil)

	ordinals, _ := json.Marshal(map[string]any{"ordinals": page.Shards[0].Ordinals})
	res := s.call(t, dev, "", http.MethodPost, vPath+"/stream", string(ordinals))
	if res.status != http.StatusOK || res.header.Get("Content-Type") != "application/x-tar" {
		t.Fatalf("stream: %s", res)
	}
	tr := tar.NewReader(bytes.NewReader(res.body))
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		names = append(names, h.Name)
		if !bytes.Equal(body, want[h.Name]) || h.PAXRecords["OSP.sha256"] != sha256Of(body) {
			t.Fatalf("%s: %d bytes, sha %s", h.Name, len(body), h.PAXRecords["OSP.sha256"])
		}
	}
	if len(names) != 7 || names[0] != "sh/item-00.txt" || names[6] != "sh/zz-loose.txt" {
		t.Fatalf("every file once, in path order: %v", names)
	}
	s.expect(t, dev, "", http.MethodPost, vPath+"/stream", `{"ordinals":[999]}`, http.StatusBadRequest, nil)
	other := s.team(t, "shards-b")
	otherTok := s.signIn(t, other.devUser.Email, other.devPW).AccessToken
	s.expect(t, otherTok, "", http.MethodPost, vPath+"/stream", string(ordinals), http.StatusNotFound, nil)

	// The second pack changes underneath: the stream is cut, never a
	// complete tar.
	packs := rig.packs(t, acme.org.ID)
	stored, _ := s.ie.storage.Object(packs[1].ObjectKey)
	broken := append([]byte{}, stored...)
	broken[0] ^= 0xff
	s.ie.storage.PutObject(packs[1].ObjectKey, broken)
	req, _ := http.NewRequest(http.MethodPost, s.api.URL+vPath+"/stream", bytes.NewReader(ordinals))
	req.Header.Set("Authorization", "Bearer "+dev)
	req.Header.Set("Content-Type", "application/json")
	if resp, err := http.DefaultClient.Do(req); err == nil {
		tr := tar.NewReader(resp.Body)
		complete := true
		for {
			_, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				complete = false
				break
			}
			if _, err := io.Copy(io.Discard, tr); err != nil {
				complete = false
				break
			}
		}
		resp.Body.Close()
		if complete {
			t.Fatal("a stream over a changed pack read as complete")
		}
	}
	s.ie.storage.PutObject(packs[1].ObjectKey, stored)

	// The signed manifest pins the snapshot's manifest SHA-256.
	var signed struct {
		Body, Algorithm, KeyID, Signature string
	}
	s.expect(t, dev, "", http.MethodGet, vPath+"/signed-manifest", "", http.StatusOK, &signed)
	sig, _ := base64.StdEncoding.DecodeString(signed.Signature)
	public := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	var body map[string]any
	_ = json.Unmarshal([]byte(signed.Body), &body)
	if !ed25519.Verify(public, []byte(signed.Body), sig) || body["manifest_sha256"] != v.ManifestSHA256 || body["version_id"] != v.ID {
		t.Fatalf("signed manifest: %+v", signed)
	}

	// The organization's pack size: owners set it.
	s.expect(t, dev, "", http.MethodPut, "/api/v1/packing-settings", `{"target_pack_bytes":1048576}`, http.StatusForbidden, nil)
	s.expect(t, owner, "", http.MethodPut, "/api/v1/packing-settings", `{"target_pack_bytes":100}`, http.StatusBadRequest, nil)
	var settings struct {
		TargetPackBytes int64 `json:"target_pack_bytes"`
	}
	s.expect(t, owner, "", http.MethodPut, "/api/v1/packing-settings", `{"target_pack_bytes":268435456}`, http.StatusOK, nil)
	s.expect(t, dev, "", http.MethodGet, "/api/v1/packing-settings", "", http.StatusOK, &settings)
	if settings.TargetPackBytes != 256<<20 {
		t.Fatalf("settings: %+v", settings)
	}

	// Repacks: one at a time, cancellable.
	var rq model.RepackRequest
	s.expect(t, owner, "", http.MethodPost, dsPath+"/repacks", "", http.StatusAccepted, &rq)
	s.expect(t, owner, "", http.MethodPost, dsPath+"/repacks", "", http.StatusConflict, nil)
	var rqs struct {
		Repacks []model.RepackRequest `json:"repacks"`
	}
	s.expect(t, dev, "", http.MethodGet, dsPath+"/repacks", "", http.StatusOK, &rqs)
	if len(rqs.Repacks) != 1 || rqs.Repacks[0].State != "PENDING" {
		t.Fatalf("repacks: %+v", rqs)
	}
	s.expect(t, owner, "", http.MethodPost, dsPath+"/repacks/"+rq.ID+"/cancel", "", http.StatusNoContent, nil)
	s.expect(t, owner, "", http.MethodPost, dsPath+"/repacks/"+rq.ID+"/cancel", "", http.StatusNotFound, nil)

	// A transformation's output can name its parent as lineage only.
	var derived response.DatasetResponse
	s.expect(t, dev, "", http.MethodPost, "/api/v1/datasets", fmt.Sprintf(`{"name":"shards-out","derived_from":{"version_ids":[%q],
		"copy_members":false,"transformation":{"name":"dedupe","version":"1"}}}`, v.ID), http.StatusCreated, &derived)
	if derived.MemberCount != 0 || derived.Derivation == nil || !derived.Derivation.Empty || derived.Derivation.VersionIDs[0] != v.ID {
		t.Fatalf("an empty derived dataset: %+v", derived)
	}

	// A run's verification reports and checkpoints.
	var run struct {
		ID string `json:"id"`
	}
	s.expect(t, dev, "", http.MethodPost, "/api/v1/training-runs",
		fmt.Sprintf(`{"name":"verify-run","source":"sdk","dataset_version_ids":[%q]}`, v.ID), http.StatusCreated, &run)
	runPath := "/api/v1/training-runs/" + run.ID
	s.expect(t, dev, "", http.MethodPost, runPath+"/verifications", `{"host":"gpu-1","sample_rate":0,"checked":1,"failed":0}`, http.StatusBadRequest, nil)
	s.expect(t, dev, "", http.MethodPost, runPath+"/verifications", fmt.Sprintf(`{"version_id":%q,"host":"gpu-1","sample_rate":0.1,
		"checked":70,"failed":1,"failures":[{"path":"sh/item-03.txt","reason":"sha256 mismatch"}]}`, v.ID), http.StatusCreated, nil)
	var verifications struct {
		Verifications []model.RunVerification `json:"verifications"`
	}
	s.expect(t, dev, "", http.MethodGet, runPath+"/verifications", "", http.StatusOK, &verifications)
	if len(verifications.Verifications) != 1 || verifications.Verifications[0].Failures[0].Reason != "sha256 mismatch" {
		t.Fatalf("verifications: %+v", verifications)
	}

	var cp model.RunCheckpoint
	s.expect(t, dev, "", http.MethodPost, runPath+"/checkpoints", fmt.Sprintf(`{"step":100,"version_id":%q,"host":"gpu-1"}`, v.ID), http.StatusCreated, &cp)
	s.expect(t, dev, "", http.MethodPost, runPath+"/checkpoints", fmt.Sprintf(`{"step":100,"version_id":%q,"host":"gpu-1"}`, v.ID), http.StatusConflict, nil)
	s.expect(t, dev, "", http.MethodPost, runPath+"/checkpoints/"+cp.ID+"/verifications", `{"host":"GPU-1"}`, http.StatusConflict, nil)
	s.expect(t, dev, "", http.MethodPost, runPath+"/checkpoints/"+cp.ID+"/verifications", `{"host":"gpu-2"}`, http.StatusOK, nil)
	var later model.RunCheckpoint
	s.expect(t, dev, "", http.MethodPost, runPath+"/checkpoints", fmt.Sprintf(`{"step":200,"version_id":%q,"host":"gpu-1"}`, v.ID), http.StatusCreated, &later)
	s.expect(t, dev, "", http.MethodPost, runPath+"/checkpoints/"+later.ID+"/verifications",
		`{"host":"gpu-3","failures":[{"path":"model.pt","reason":"sha256 mismatch"}]}`, http.StatusOK, nil)
	var cps struct {
		Checkpoints  []model.RunCheckpoint `json:"checkpoints"`
		LastVerified *model.RunCheckpoint  `json:"last_verified"`
	}
	s.expect(t, dev, "", http.MethodGet, runPath+"/checkpoints", "", http.StatusOK, &cps)
	if len(cps.Checkpoints) != 2 || cps.LastVerified == nil || cps.LastVerified.Step != 100 || cps.LastVerified.VerifiedBy != "gpu-2" ||
		cps.Checkpoints[0].VerifyError == "" {
		t.Fatalf("roll back to step 100: %+v", cps)
	}
}

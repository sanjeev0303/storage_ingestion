package tiering

import (
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

func TestEstimatesFollowTheListPrices(t *testing.T) {
	aws := model.PricesFor(model.ProviderAWSS3)
	tb := int64(1) << 40

	// Downloading 1 TiB of Standard: 1024 GB of egress at $0.09.
	d := estimate(EstimateInput{Operation: "download", Files: 1000, Bytes: tb}, aws)
	if d.EgressUSD != 92.16 || d.RetrievalUSD != 0 || d.Requests != 1000 {
		t.Fatalf("download: %+v", d)
	}
	// From Standard-IA it also pays retrieval.
	if d := estimate(EstimateInput{Operation: "download", Bytes: tb, Tier: model.TierInfrequent}, aws); d.RetrievalUSD != 10.24 {
		t.Fatalf("retrieval: %+v", d)
	}
	// An epoch over a packed snapshot makes the reads it is told to.
	e := estimate(EstimateInput{Operation: "epoch", Files: 1_000_000, Requests: 2_000, Bytes: tb, Epochs: 10}, aws)
	if e.Requests != 20_000 || e.EgressUSD != 0 {
		t.Fatalf("epoch: %+v", e)
	}
	// Ten years of Deep Archive, against Standard.
	cold := estimate(EstimateInput{Operation: "retain", Bytes: tb, Tier: model.TierDeepArchive, Years: 10}, aws)
	hot := estimate(EstimateInput{Operation: "retain", Bytes: tb, Years: 10}, aws)
	if cold.StorageUSD != 121.65 || hot.StorageUSD != 2826.24 {
		t.Fatalf("retain: cold %+v hot %+v", cold, hot)
	}
	// A short retention still bills the tier's minimum days.
	if m := estimate(EstimateInput{Operation: "retain", Bytes: gb, Tier: model.TierDeepArchive, Years: 0.1}, aws); m.StorageUSD != 0.01 {
		t.Fatalf("minimum days: %+v", m)
	}
	// A fast restore costs more and is quicker.
	fast := estimate(EstimateInput{Operation: "restore", Files: 1, Bytes: tb, Speed: model.RestoreFast}, aws)
	bulk := estimate(EstimateInput{Operation: "restore", Files: 1, Bytes: tb, Speed: model.RestoreBulk}, aws)
	if fast.RetrievalUSD <= bulk.RetrievalUSD || fast.Hours >= bulk.Hours {
		t.Fatalf("restore: fast %+v bulk %+v", fast, bulk)
	}
	// R2 has no egress fees; an unknown store prices as S3, and says so.
	if r2 := estimate(EstimateInput{Operation: "download", Bytes: tb}, model.PricesFor(model.ProviderR2)); r2.EgressUSD != 0 {
		t.Fatalf("r2: %+v", r2)
	}
	if p := model.PricesFor(model.ProviderS3Compatible); p.Note == aws.Note || p.EgressGB != aws.EgressGB {
		t.Fatalf("self-hosted: %+v", p)
	}
}

func TestWarmerTiers(t *testing.T) {
	got := warmerThan(model.TierArchive)
	if len(got) != 2 || got[0] != "STANDARD" || got[1] != "INFREQUENT" {
		t.Fatalf("warmer than ARCHIVE: %v", got)
	}
}

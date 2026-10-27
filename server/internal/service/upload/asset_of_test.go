package upload

import (
	"context"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/repository/memory"
)

func TestAssetOfReportsWhatTheUploadBecame(t *testing.T) {
	ctx := context.Background()
	assets := memory.NewAssetRepository()
	rejected := &model.Asset{ID: "a1", TenantID: "t1", OwnerID: "o1", UploadID: "u1",
		ObjectKey: "quarantine/t1/u1", FileName: "notes.txt", ContentType: "text/plain", SizeBytes: 68,
		Status: model.AssetStatusRejected, RejectionReason: model.RejectMalwareDetected}
	if err := assets.Create(ctx, rejected); err != nil {
		t.Fatal(err)
	}

	without := &Service{}
	if got, err := without.AssetOf(ctx, "t1", "u1"); got != nil || err != nil {
		t.Fatalf("no reader: got %v, %v; want nil, nil", got, err)
	}

	s := (&Service{}).ReportAssets(assets)
	got, err := s.AssetOf(ctx, "t1", "u1")
	if err != nil || got == nil || got.Status != model.AssetStatusRejected || got.RejectionReason != model.RejectMalwareDetected {
		t.Fatalf("got %+v, %v; want the rejected asset", got, err)
	}
	if got, err := s.AssetOf(ctx, "t1", "u2"); got != nil || err != nil {
		t.Fatalf("not processed yet: got %v, %v; want nil, nil", got, err)
	}
	if got, err := s.AssetOf(ctx, "t2", "u1"); got != nil || err != nil {
		t.Fatalf("another tenant: got %v, %v; want nil, nil", got, err)
	}
}

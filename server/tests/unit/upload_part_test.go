package unit

import (
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

func TestUploadPartValidate(t *testing.T) {
	tests := []struct {
		name    string
		part    model.UploadPart
		wantErr bool
	}{
		{
			name: "valid uploaded part",
			part: model.UploadPart{
				TenantID:   "tenant-1",
				UploadID:   "upload-1",
				PartNumber: 1,
				ETag:       `"part-etag"`,
				SizeBytes:  5 * 1024 * 1024,
				Status:     model.UploadPartUploaded,
			},
		},
		{
			name: "invalid part number",
			part: model.UploadPart{
				TenantID:   "tenant-1",
				UploadID:   "upload-1",
				PartNumber: 0,
				Status:     model.UploadPartPending,
			},
			wantErr: true,
		},
		{
			name: "uploaded part without ETag",
			part: model.UploadPart{
				TenantID:   "tenant-1",
				UploadID:   "upload-1",
				PartNumber: 1,
				SizeBytes:  1024,
				Status:     model.UploadPartUploaded,
			},
			wantErr: true,
		},
		{
			name: "invalid status",
			part: model.UploadPart{
				TenantID:   "tenant-1",
				UploadID:   "upload-1",
				PartNumber: 1,
				Status:     model.UploadPartStatus("UNKNOWN"),
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.part.Validate()

			if tt.wantErr && err == nil {
				t.Fatal("expected validation error, got nil")
			}

			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}

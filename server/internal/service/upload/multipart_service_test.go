package upload

import (
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

func TestValidateCompletion(t *testing.T) {
	const partSize int64 = 5 * 1024 * 1024

	session := model.UploadSession{
		ID:           "upload-1",
		TenantID:     "tenant-1",
		OwnerID:      "user-1",
		ExpectedSize: 2 * partSize,
		ExpiresAt:    time.Now().UTC().Add(time.Hour),
	}

	recorded := []model.UploadPart{
		{
			TenantID:   "tenant-1",
			UploadID:   "upload-1",
			PartNumber: 1,
			ETag:       `"etag-1"`,
			SizeBytes:  partSize,
			Status:     model.UploadPartUploaded,
		},
		{
			TenantID:   "tenant-1",
			UploadID:   "upload-1",
			PartNumber: 2,
			ETag:       `"etag-2"`,
			SizeBytes:  partSize,
			Status:     model.UploadPartUploaded,
		},
	}

	tests := []struct {
		name    string
		input   CompleteMultipartInput
		wantErr bool
	}{
		{
			name: "valid ordered parts",
			input: CompleteMultipartInput{
				Parts: []CompletedPartInput{
					{PartNumber: 1, ETag: `"etag-1"`},
					{PartNumber: 2, ETag: `"etag-2"`},
				},
			},
		},
		{
			name: "wrong order",
			input: CompleteMultipartInput{
				Parts: []CompletedPartInput{
					{PartNumber: 2, ETag: `"etag-2"`},
					{PartNumber: 1, ETag: `"etag-1"`},
				},
			},
			wantErr: true,
		},
		{
			name: "wrong ETag",
			input: CompleteMultipartInput{
				Parts: []CompletedPartInput{
					{PartNumber: 1, ETag: `"wrong-etag"`},
					{PartNumber: 2, ETag: `"etag-2"`},
				},
			},
			wantErr: true,
		},
		{
			name: "missing part",
			input: CompleteMultipartInput{
				Parts: []CompletedPartInput{
					{PartNumber: 1, ETag: `"etag-1"`},
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateCompletion(session, recorded, tt.input)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected validation error")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(got) != len(recorded) {
				t.Fatalf("got %d completed parts, want %d", len(got), len(recorded))
			}
		})
	}
}

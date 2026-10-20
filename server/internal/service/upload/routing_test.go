package upload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/request"
	storagememory "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/memory"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/repository/memory"
)

// tenantStorage is one tenant connection on an in-memory provider; it
// both chooses and resolves.
type tenantStorage struct {
	storage *storagememory.ObjectStorage
	caps    model.ProviderCapabilities
	staging string // the staging connection a new upload gets
	asked   []string
}

func (s *tenantStorage) location(id string) interfaces.StorageLocation {
	return interfaces.StorageLocation{
		StoragePlace: interfaces.StoragePlace{ConnectionID: id, StagingBucket: "customer", StagingPrefix: "staging",
			ReadyBucket: "customer", ReadyPrefix: "published"},
		Storage: s.storage, Capabilities: s.caps,
	}
}

func (s *tenantStorage) ChooseForUpload(_ context.Context, p model.Principal, requested, _ string) (interfaces.StorageChoice, error) {
	s.asked = append(s.asked, requested)
	if requested == "sc_other" {
		return interfaces.StorageChoice{}, model.ErrConnectionNotFound
	}
	if requested == model.PlatformConnectionID {
		platform := interfaces.StorageLocation{StoragePlace: interfaces.StoragePlace{ConnectionID: model.PlatformConnectionID}}
		return interfaces.StorageChoice{Final: platform, Staging: platform}, nil
	}
	staging := s.location("sc_customer")
	if s.staging == model.PlatformConnectionID {
		staging = interfaces.StorageLocation{StoragePlace: interfaces.StoragePlace{ConnectionID: model.PlatformConnectionID}}
	}
	return interfaces.StorageChoice{Final: s.location("sc_customer"), Staging: staging}, nil
}

func (s *tenantStorage) Locate(_ context.Context, tenantID, connectionID string) (interfaces.StorageLocation, error) {
	if connectionID != "sc_customer" || tenantID != testTenant {
		return interfaces.StorageLocation{}, model.ErrConnectionNotFound
	}
	return s.location(connectionID), nil
}

func singleRequest(key string) request.CreateUploadRequest {
	return request.CreateUploadRequest{FileName: "a.bin", ContentType: "application/octet-stream", SizeBytes: 4, IdempotencyKey: key}
}

func TestSingleUploadIsBoundToTheChosenConnection(t *testing.T) {
	platform := storagememory.NewObjectStorage()
	tenant := &tenantStorage{storage: storagememory.NewObjectStorage(), caps: model.ProviderCapabilities{PresignedUpload: true, MaxSingleUploadBytes: 1 << 20}}
	svc := NewService(memory.NewUploadRepository(), platform, "platform-bucket", 1<<30, time.Hour).Route(tenant, tenant)
	ctx := context.Background()

	result, err := svc.Create(ctx, CreateInput{TenantID: testTenant, OwnerID: testOwner, Request: singleRequest("k1")})
	if err != nil {
		t.Fatal(err)
	}
	session := result.Upload
	if session.StorageConnectionID != "sc_customer" || session.StagingConnectionID != "sc_customer" ||
		session.ObjectKey != "staging/"+testTenant+"/"+session.ID {
		t.Fatalf("the binding and key come from the chosen connection: %+v", session)
	}
	if !strings.Contains(result.Plan.UploadURL, "staging/"+testTenant) {
		t.Fatalf("the upload is signed by the tenant's storage: %s", result.Plan.UploadURL)
	}

	// Completion checks the object where the binding says, never the
	// platform's bucket.
	tenant.storage.PutObject(session.ObjectKey, []byte("data"))
	if status, err := svc.Complete(ctx, testTenant, session.ID); err != nil || status != model.UploadStatusCompleted {
		t.Fatalf("complete on the tenant connection: %s %v", status, err)
	}

	// A retry asking for another connection is a different request.
	req := singleRequest("k1")
	req.StorageConnectionID = model.PlatformConnectionID
	if _, err := svc.Create(ctx, CreateInput{TenantID: testTenant, OwnerID: testOwner, Request: req}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("idempotency covers the connection: %v", err)
	}
	if _, err := svc.Create(ctx, CreateInput{TenantID: testTenant, OwnerID: testOwner, Request: singleRequest("k1")}); err != nil {
		t.Fatalf("a plain retry replays the bound session: %v", err)
	}

	req = singleRequest("k2")
	req.StorageConnectionID = "sc_other"
	if _, err := svc.Create(ctx, CreateInput{TenantID: testTenant, OwnerID: testOwner, Request: req}); !errors.Is(err, model.ErrConnectionNotFound) {
		t.Fatalf("another tenant's connection: %v", err)
	}
}

func TestSingleUploadRespectsConnectionCapabilities(t *testing.T) {
	tenant := &tenantStorage{storage: storagememory.NewObjectStorage(), caps: model.ProviderCapabilities{PresignedUpload: true, MaxSingleUploadBytes: 3}}
	svc := NewService(memory.NewUploadRepository(), storagememory.NewObjectStorage(), "platform-bucket", 1<<30, time.Hour).Route(tenant, tenant)
	if _, err := svc.Create(context.Background(), CreateInput{TenantID: testTenant, OwnerID: testOwner, Request: singleRequest("k")}); !errors.Is(err, ErrUploadTooLarge) {
		t.Fatalf("above the provider's single-upload limit: %v", err)
	}
	tenant.caps = model.ProviderCapabilities{}
	if _, err := svc.Create(context.Background(), CreateInput{TenantID: testTenant, OwnerID: testOwner, Request: singleRequest("k2")}); !errors.Is(err, model.ErrUnsupportedProvider) {
		t.Fatalf("a connection that cannot sign uploads: %v", err)
	}
}

func TestWithoutRoutingOnlyThePlatformIsAccepted(t *testing.T) {
	svc := NewService(memory.NewUploadRepository(), storagememory.NewObjectStorage(), "platform-bucket", 1<<30, time.Hour)
	req := singleRequest("k")
	req.StorageConnectionID = "sc_customer"
	_, err := svc.Create(context.Background(), CreateInput{TenantID: testTenant, OwnerID: testOwner, Request: req})
	if !errors.Is(err, ErrInvalidRequest) || !errors.Is(err, model.ErrStorageNotConfigured) {
		t.Fatalf("no connections without routing: %v", err)
	}
	req.StorageConnectionID = model.PlatformConnectionID
	if result, err := svc.Create(context.Background(), CreateInput{TenantID: testTenant, OwnerID: testOwner, Request: req}); err != nil ||
		result.Upload.ObjectKey != "quarantine/"+testTenant+"/"+result.Upload.ID {
		t.Fatalf("the platform as before: %+v %v", result.Upload, err)
	}
}

func TestMultipartOnAzureStyleBlocksChecksSizesNotETags(t *testing.T) {
	h := newHarness(t)
	tenant := &tenantStorage{storage: storagememory.NewObjectStorage(),
		caps: model.ProviderCapabilities{Multipart: true, PartValidator: "BLOCK_ID"}}
	h.svc.Route(tenant, tenant)
	ctx := context.Background()

	data := bytes.Repeat([]byte("b"), 1024)
	sum := sha256.Sum256(data)
	result, err := h.svc.Start(ctx, StartMultipartInput{
		TenantID: testTenant, OwnerID: testOwner, FileName: "blocks.bin", ContentType: "application/octet-stream",
		SizeBytes: int64(len(data)), ChecksumSHA256: hex.EncodeToString(sum[:]), IdempotencyKey: "blocks",
	})
	if err != nil {
		t.Fatal(err)
	}
	session := result.Session
	if session.ObjectKey != "staging/"+testTenant+"/"+session.ID || h.storage.UploadCount() != 0 || tenant.storage.UploadCount() != 1 {
		t.Fatalf("the provider session is on the tenant connection: %+v", session)
	}

	// Two small parts: no S3 5 MiB minimum, and the provider's validator
	// is a block ID, so the client has no content ETag to confirm.
	tenant.storage.PutPart(session.StorageUploadID, 1, data[:512])
	tenant.storage.PutPart(session.StorageUploadID, 2, data[512:])
	err = h.svc.RecordParts(ctx, testTenant, testOwner, session.ID, []RecordPartInput{{PartNumber: 1, SizeBytes: 512}, {PartNumber: 2, SizeBytes: 512}})
	if err != nil {
		t.Fatalf("block parts are confirmed by number and size: %v", err)
	}
	err = h.svc.RecordParts(ctx, testTenant, testOwner, session.ID, []RecordPartInput{{PartNumber: 1, SizeBytes: 511}})
	if !errors.Is(err, ErrInvalidPart) {
		t.Fatalf("a wrong size is still refused: %v", err)
	}

	done, err := h.svc.Complete(ctx, testTenant, testOwner, session.ID, CompleteMultipartInput{Parts: []CompletedPartInput{{PartNumber: 1}, {PartNumber: 2}}})
	if err != nil || done.Status != model.UploadStatusCompleted {
		t.Fatalf("complete: %+v %v", done, err)
	}
	if got, ok := tenant.storage.Object(session.ObjectKey); !ok || !bytes.Equal(got, data) {
		t.Fatal("the object is assembled on the tenant connection")
	}
	if status, err := h.svc.VerifyIntegrity(ctx, testTenant, session.ID); err != nil || status != model.UploadStatusVerified {
		t.Fatalf("the SHA-256 is verified on the tenant connection: %s %v", status, err)
	}
}

func TestMultipartContentValidatorStillChecksETags(t *testing.T) {
	h := newHarness(t)
	tenant := &tenantStorage{storage: storagememory.NewObjectStorage(),
		caps: model.ProviderCapabilities{Multipart: true, PartValidator: "CONTENT", MinPartBytes: 5 << 20}}
	h.svc.Route(tenant, tenant)
	ctx := context.Background()
	data := []byte("content")
	sum := sha256.Sum256(data)
	result, err := h.svc.Start(ctx, StartMultipartInput{
		TenantID: testTenant, OwnerID: testOwner, FileName: "c.bin", ContentType: "application/octet-stream",
		SizeBytes: int64(len(data)), ChecksumSHA256: hex.EncodeToString(sum[:]), IdempotencyKey: "content",
	})
	if err != nil {
		t.Fatal(err)
	}
	tenant.storage.PutPart(result.Session.StorageUploadID, 1, data)
	err = h.svc.RecordParts(ctx, testTenant, testOwner, result.Session.ID, []RecordPartInput{{PartNumber: 1, ETag: `"wrong"`, SizeBytes: int64(len(data))}})
	if !errors.Is(err, ErrInvalidPart) {
		t.Fatalf("a content validator must match: %v", err)
	}
}

func TestMultipartRefusesAConnectionWithoutMultipart(t *testing.T) {
	h := newHarness(t)
	tenant := &tenantStorage{storage: storagememory.NewObjectStorage()}
	h.svc.Route(tenant, tenant)
	sum := sha256.Sum256([]byte("x"))
	_, err := h.svc.Start(context.Background(), StartMultipartInput{
		TenantID: testTenant, OwnerID: testOwner, FileName: "x", ContentType: "text/plain", SizeBytes: 1,
		ChecksumSHA256: hex.EncodeToString(sum[:]), IdempotencyKey: "x",
	})
	if !errors.Is(err, model.ErrUnsupportedProvider) {
		t.Fatalf("multipart needs the capability: %v", err)
	}
}

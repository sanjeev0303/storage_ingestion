package integration

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"

	s3storage "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/s3"
)

// TestProviderObjectLock runs the Object Lock calls against the real
// provider: a locked version cannot be deleted while held or retained.
func TestProviderObjectLock(t *testing.T) {
	_, plainBucket := openTestProvider(t)
	if os.Getenv("TEST_S3_CREATE_BUCKET") != "true" {
		t.Skip("needs TEST_S3_CREATE_BUCKET=true to create a locked test bucket")
	}
	cfg, err := s3storage.LoadConfig()
	if err != nil {
		t.Skip("S3 configuration incomplete")
	}
	ctx := context.Background()
	client, err := s3storage.NewClient(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	bucket := plainBucket + "-locked"
	if _, err := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)}); err != nil {
		if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket), ObjectLockEnabledForBucket: aws.Bool(true)}); err != nil {
			t.Skipf("this provider cannot create a bucket with Object Lock: %v", err)
		}
	}
	storage := s3storage.NewObjectStorage(client)
	if on, err := storage.ObjectLockEnabled(ctx, bucket); err != nil || !on {
		t.Fatalf("locked bucket: %v %v", on, err)
	}
	if on, err := storage.ObjectLockEnabled(ctx, plainBucket); err != nil || on {
		t.Fatalf("plain bucket: %v %v", on, err)
	}

	key := fmt.Sprintf("locked/%d", time.Now().UnixNano())
	out, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader([]byte("worm"))})
	if err != nil {
		t.Fatal(err)
	}
	version := aws.ToString(out.VersionId)
	until := time.Now().Add(4 * time.Second).Truncate(time.Second)
	if err := storage.LockObjectVersion(ctx, bucket, key, version, interfaces.VersionLock{Mode: "GOVERNANCE", Until: until, LegalHold: true}); err != nil {
		t.Fatal(err)
	}
	if err := storage.DeleteObjectVersion(ctx, bucket, key, version); err == nil {
		t.Fatal("a held, retained version was deleted")
	}
	// The provider's own record, read back, is what was set.
	got, err := storage.ObjectLockStatus(ctx, bucket, key, version)
	if err != nil || got.Mode != "GOVERNANCE" || !got.LegalHold || !got.Until.Equal(until.UTC()) {
		t.Fatalf("read back: %+v %v", got, err)
	}
	// A lock in force cannot change its mode without the governance
	// bypass, which the platform never sends.
	if err := storage.LockObjectVersion(ctx, bucket, key, version, interfaces.VersionLock{Mode: "COMPLIANCE", Until: until}); err == nil {
		t.Fatal("the provider changed a lock's mode without the bypass")
	}
	// The legal hold comes off; the retention stays.
	if err := storage.LockObjectVersion(ctx, bucket, key, version, interfaces.VersionLock{}); err != nil {
		t.Fatal(err)
	}

	// COMPLIANCE on a new version: read back as set; the provider refuses a
	// downgrade and a delete until its date.
	out2, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key + "-c"), Body: bytes.NewReader([]byte("worm"))})
	if err != nil {
		t.Fatal(err)
	}
	cv := aws.ToString(out2.VersionId)
	if err := storage.LockObjectVersion(ctx, bucket, key+"-c", cv, interfaces.VersionLock{Mode: "COMPLIANCE", Until: until}); err != nil {
		t.Fatal(err)
	}
	if got, err := storage.ObjectLockStatus(ctx, bucket, key+"-c", cv); err != nil || got.Mode != "COMPLIANCE" {
		t.Fatalf("COMPLIANCE read back: %+v %v", got, err)
	}
	if err := storage.LockObjectVersion(ctx, bucket, key+"-c", cv, interfaces.VersionLock{Mode: "GOVERNANCE", Until: until}); err == nil {
		t.Fatal("the provider let COMPLIANCE be downgraded")
	}
	if err := storage.DeleteObjectVersion(ctx, bucket, key+"-c", cv); err == nil {
		t.Fatal("a COMPLIANCE version was deleted before its date")
	}
	defer func() { _ = storage.DeleteObjectVersion(context.Background(), bucket, key+"-c", cv) }()
	if err := storage.DeleteObjectVersion(ctx, bucket, key, version); err == nil {
		t.Fatal("a retained version was deleted before its date")
	}
	time.Sleep(time.Until(until) + time.Second)
	if err := storage.DeleteObjectVersion(ctx, bucket, key, version); err != nil {
		t.Fatalf("delete after the retention date: %v", err)
	}
}

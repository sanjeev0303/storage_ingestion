package s3

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// UseKMSKey encrypts every write, copy and presigned upload with an
// SSE-KMS key the customer manages.
func (s *ObjectStorage) UseKMSKey(key string) { s.kmsKey = key }

// EnableAccountChecks marks the client as AWS's own S3, where the caller's
// identity and the bucket's owner can be asked of STS and S3.
func (s *ObjectStorage) EnableAccountChecks() { s.aws = true }

// UsePlain sets the client without the bucket-owner header, for presigned
// URLs (which a browser could not send the header with) and for calls
// that name no bucket.
func (s *ObjectStorage) UsePlain(plain *s3.Client) {
	s.plain = plain
	s.presigner = s3.NewPresignClient(plain)
}

func (s *ObjectStorage) plainClient() *s3.Client {
	if s.plain != nil {
		return s.plain
	}
	return s.client
}

// expectedOwner sends x-amz-expected-bucket-owner on every request, so a
// bucket that has changed hands (deleted, then created by someone else
// with the same name) refuses the platform instead of receiving its data.
func expectedOwner(account string) func(*middleware.Stack) error {
	return func(stack *middleware.Stack) error {
		return stack.Build.Add(middleware.BuildMiddlewareFunc("ExpectedBucketOwner",
			func(ctx context.Context, in middleware.BuildInput, next middleware.BuildHandler) (middleware.BuildOutput, middleware.Metadata, error) {
				if req, ok := in.Request.(*smithyhttp.Request); ok && req.Header.Get("X-Amz-Expected-Bucket-Owner") == "" {
					req.Header.Set("X-Amz-Expected-Bucket-Owner", account)
				}
				return next.HandleBuild(ctx, in)
			}), middleware.After)
	}
}

func apiCode(err error) string {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode()
	}
	return ""
}

// unanswerable reports errors that mean the question cannot be asked:
// the provider does not implement the call, or the credentials may not.
func unanswerable(err error) (string, bool) {
	switch code := apiCode(err); code {
	case "NotImplemented", "MethodNotAllowed", "XNotImplemented", "UnsupportedOperation":
		return "the provider does not support this check", true
	case "AccessDenied", "Forbidden", "AllAccessDisabled":
		return "the credentials may not read this setting", true
	}
	var withStatus interface{ HTTPStatusCode() int }
	if errors.As(err, &withStatus) && (withStatus.HTTPStatusCode() == 501 || withStatus.HTTPStatusCode() == 405) {
		return "the provider does not support this check", true
	}
	return "", false
}

type postureRun struct {
	facts model.PostureFacts
}

func (r *postureRun) add(name, status, detail string) {
	r.facts.Checks = append(r.facts.Checks, model.PostureCheck{Name: name, Status: status, Detail: detail})
}

// skip records a check that could not be answered, or returns false for
// an error that is a real failure.
func (r *postureRun) skip(name string, err error, why string) bool {
	if reason, ok := unanswerable(err); ok {
		r.add(name, model.PostureNotChecked, reason+why)
		return true
	}
	return false
}

// Posture describes the bucket's security settings. Every check reads a
// setting; nothing is changed. A check the provider or the credentials
// cannot answer is NOT_CHECKED, never a pass.
func (s *ObjectStorage) Posture(ctx context.Context, bucket string, opts model.PostureOptions) (model.PostureFacts, error) {
	r := &postureRun{}
	b := aws.String(bucket)

	if err := s.publicAccessBlock(ctx, r, b); err != nil {
		return r.facts, err
	}
	if err := s.publicPolicy(ctx, r, b); err != nil {
		return r.facts, err
	}
	if err := s.publicACL(ctx, r, b); err != nil {
		return r.facts, err
	}
	if s.aws {
		if err := s.ownership(ctx, r, b); err != nil {
			return r.facts, err
		}
	}
	if err := s.encryption(ctx, r, b); err != nil {
		return r.facts, err
	}
	if err := s.versioningCheck(ctx, r, b); err != nil {
		return r.facts, err
	}
	if err := s.objectLockCheck(ctx, r, b); err != nil {
		return r.facts, err
	}
	if err := s.lifecycle(ctx, r, b, opts.Prefixes); err != nil {
		return r.facts, err
	}
	s.location(ctx, r, b)
	s.credentialScope(ctx, r)
	if s.aws {
		s.bucketOwner(ctx, r, b, opts.ExpectedOwner)
	}
	return r.facts, nil
}

func (s *ObjectStorage) publicAccessBlock(ctx context.Context, r *postureRun, b *string) error {
	out, err := s.client.GetPublicAccessBlock(ctx, &s3.GetPublicAccessBlockInput{Bucket: b})
	switch {
	case apiCode(err) == "NoSuchPublicAccessBlockConfiguration":
		r.add(model.CheckPublicAccessBlock, model.PostureWarn, "Block Public Access is not set on the bucket; turn on all four settings")
		return nil
	case err != nil:
		if r.skip(model.CheckPublicAccessBlock, err, "") {
			return nil
		}
		return classify("get public access block", err)
	}
	c := out.PublicAccessBlockConfiguration
	var missing []string
	if c == nil || !aws.ToBool(c.BlockPublicAcls) {
		missing = append(missing, "BlockPublicAcls")
	}
	if c == nil || !aws.ToBool(c.IgnorePublicAcls) {
		missing = append(missing, "IgnorePublicAcls")
	}
	if c == nil || !aws.ToBool(c.BlockPublicPolicy) {
		missing = append(missing, "BlockPublicPolicy")
	}
	if c == nil || !aws.ToBool(c.RestrictPublicBuckets) {
		missing = append(missing, "RestrictPublicBuckets")
	}
	if len(missing) > 0 {
		r.add(model.CheckPublicAccessBlock, model.PostureWarn, "Block Public Access is partly off: turn on "+strings.Join(missing, ", "))
		return nil
	}
	r.add(model.CheckPublicAccessBlock, model.PosturePass, "all four Block Public Access settings are on")
	return nil
}

func (s *ObjectStorage) publicPolicy(ctx context.Context, r *postureRun, b *string) error {
	out, err := s.client.GetBucketPolicyStatus(ctx, &s3.GetBucketPolicyStatusInput{Bucket: b})
	switch {
	case apiCode(err) == "NoSuchBucketPolicy":
		r.add(model.CheckPublicPolicy, model.PosturePass, "the bucket has no bucket policy")
		return nil
	case err == nil:
		if out.PolicyStatus != nil && aws.ToBool(out.PolicyStatus.IsPublic) {
			r.add(model.CheckPublicPolicy, model.PostureFail, "the bucket policy makes the bucket public; remove the statements that allow everyone")
		} else {
			r.add(model.CheckPublicPolicy, model.PosturePass, "the bucket policy is not public")
		}
		return nil
	}
	// Providers without a policy status: read the policy itself.
	if _, ok := unanswerable(err); !ok {
		return classify("get bucket policy status", err)
	}
	policy, err := s.client.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{Bucket: b})
	switch {
	case apiCode(err) == "NoSuchBucketPolicy":
		r.add(model.CheckPublicPolicy, model.PosturePass, "the bucket has no bucket policy")
	case err != nil:
		if r.skip(model.CheckPublicPolicy, err, "") {
			return nil
		}
		return classify("get bucket policy", err)
	case policyAllowsEveryone(aws.ToString(policy.Policy)):
		r.add(model.CheckPublicPolicy, model.PostureFail, "the bucket policy allows everyone (Principal \"*\") without a condition")
	default:
		r.add(model.CheckPublicPolicy, model.PosturePass, "the bucket policy allows no one without a condition")
	}
	return nil
}

type policyDoc struct {
	Statement []struct {
		Effect    string          `json:"Effect"`
		Principal json.RawMessage `json:"Principal"`
		Action    json.RawMessage `json:"Action"`
		Condition json.RawMessage `json:"Condition"`
	} `json:"Statement"`
}

func everyone(principal json.RawMessage) bool {
	p := strings.ReplaceAll(string(principal), " ", "")
	return p == `"*"` || strings.Contains(p, `"AWS":"*"`) || strings.Contains(p, `"AWS":["*"]`)
}

// policyAllowsEveryone finds an Allow to everyone with no condition.
func policyAllowsEveryone(text string) bool {
	var doc policyDoc
	if json.Unmarshal([]byte(text), &doc) != nil {
		return false
	}
	for _, st := range doc.Statement {
		if st.Effect == "Allow" && everyone(st.Principal) && len(st.Condition) == 0 {
			return true
		}
	}
	return false
}

// policyDeniesSSEC finds a Deny on writes that carry a customer key.
func policyDeniesSSEC(text string) bool {
	var doc policyDoc
	if json.Unmarshal([]byte(text), &doc) != nil {
		return false
	}
	for _, st := range doc.Statement {
		if st.Effect == "Deny" && strings.Contains(strings.ToLower(string(st.Condition)), "x-amz-server-side-encryption-customer-algorithm") {
			return true
		}
	}
	return false
}

func (s *ObjectStorage) publicACL(ctx context.Context, r *postureRun, b *string) error {
	out, err := s.client.GetBucketAcl(ctx, &s3.GetBucketAclInput{Bucket: b})
	if err != nil {
		if r.skip(model.CheckPublicACL, err, "") {
			return nil
		}
		return classify("get bucket acl", err)
	}
	for _, g := range out.Grants {
		if g.Grantee == nil {
			continue
		}
		uri := aws.ToString(g.Grantee.URI)
		if strings.HasSuffix(uri, "/global/AllUsers") || strings.HasSuffix(uri, "/global/AuthenticatedUsers") {
			r.add(model.CheckPublicACL, model.PostureFail, fmt.Sprintf("the bucket ACL grants %s to %s; remove the grant",
				g.Permission, uri[strings.LastIndex(uri, "/")+1:]))
			return nil
		}
	}
	r.add(model.CheckPublicACL, model.PosturePass, "the bucket ACL grants nothing to everyone")
	return nil
}

func (s *ObjectStorage) ownership(ctx context.Context, r *postureRun, b *string) error {
	out, err := s.client.GetBucketOwnershipControls(ctx, &s3.GetBucketOwnershipControlsInput{Bucket: b})
	switch {
	case apiCode(err) == "OwnershipControlsNotFoundError":
		r.add(model.CheckOwnership, model.PostureWarn, "ACLs are enabled; set Object Ownership to \"Bucket owner enforced\"")
		return nil
	case err != nil:
		if r.skip(model.CheckOwnership, err, "") {
			return nil
		}
		return classify("get ownership controls", err)
	}
	for _, rule := range out.OwnershipControls.Rules {
		if rule.ObjectOwnership == types.ObjectOwnershipBucketOwnerEnforced {
			r.add(model.CheckOwnership, model.PosturePass, "ACLs are disabled (bucket owner enforced)")
			return nil
		}
	}
	r.add(model.CheckOwnership, model.PostureWarn, "ACLs are enabled; set Object Ownership to \"Bucket owner enforced\"")
	return nil
}

func (s *ObjectStorage) encryption(ctx context.Context, r *postureRun, b *string) error {
	out, err := s.client.GetBucketEncryption(ctx, &s3.GetBucketEncryptionInput{Bucket: b})
	sseCBlocked := false
	switch {
	case apiCode(err) == "ServerSideEncryptionConfigurationNotFoundError":
		r.add(model.CheckEncryption, model.PostureWarn, "no default encryption is set; objects are stored unencrypted unless each upload asks")
	case err != nil:
		if !r.skip(model.CheckEncryption, err, "") {
			return classify("get bucket encryption", err)
		}
	default:
		var algorithm string
		for _, rule := range out.ServerSideEncryptionConfiguration.Rules {
			if d := rule.ApplyServerSideEncryptionByDefault; d != nil && algorithm == "" {
				algorithm = string(d.SSEAlgorithm)
				r.facts.EncryptionKey = aws.ToString(d.KMSMasterKeyID)
			}
			if bl := rule.BlockedEncryptionTypes; bl != nil && slices.Contains(bl.EncryptionType, types.EncryptionTypeSseC) {
				sseCBlocked = true
			}
		}
		switch {
		case strings.HasPrefix(algorithm, "aws:kms"):
			r.facts.Encrypted = true
			detail := "SSE-KMS by default"
			if r.facts.EncryptionKey != "" {
				detail += " with key " + r.facts.EncryptionKey
			}
			r.add(model.CheckEncryption, model.PosturePass, detail)
		case algorithm != "":
			r.facts.Encrypted = true
			r.add(model.CheckEncryption, model.PosturePass, algorithm+" by default (keys managed by the provider)")
		default:
			r.add(model.CheckEncryption, model.PostureWarn, "no default encryption is set")
		}
	}

	if sseCBlocked {
		r.add(model.CheckSSECDenied, model.PosturePass, "the bucket blocks writes encrypted with customer-provided keys (SSE-C)")
		return nil
	}
	policy, err := s.client.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{Bucket: b})
	if err == nil && policyDeniesSSEC(aws.ToString(policy.Policy)) {
		r.add(model.CheckSSECDenied, model.PosturePass, "the bucket policy denies writes that carry a customer-provided key (SSE-C)")
		return nil
	}
	r.add(model.CheckSSECDenied, model.PostureNotChecked,
		"no SSE-C block found in the settings; run the connection test with the write probe to try one")
	return nil
}

// ProbeSSEC tries to write one small object encrypted with a random
// customer-provided key. Stolen keys have been used exactly this way to
// hold data to ransom. It reports whether the bucket accepted it; an
// accepted object is deleted at once.
func (s *ObjectStorage) ProbeSSEC(ctx context.Context, bucket, key string) (bool, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return false, err
	}
	sum := md5.Sum(secret)
	body := []byte("sse-c probe\n")
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(body), ContentLength: aws.Int64(int64(len(body))),
		SSECustomerAlgorithm: aws.String("AES256"), SSECustomerKey: aws.String(base64.StdEncoding.EncodeToString(secret)),
		SSECustomerKeyMD5: aws.String(base64.StdEncoding.EncodeToString(sum[:])),
	})
	if err == nil {
		_, _ = s.client.DeleteObject(context.WithoutCancel(ctx), &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
		return true, nil
	}
	var withStatus interface{ HTTPStatusCode() int }
	if errors.As(err, &withStatus) && withStatus.HTTPStatusCode() >= 400 && withStatus.HTTPStatusCode() < 500 {
		return false, nil
	}
	return false, classify("sse-c probe", err)
}

func (s *ObjectStorage) versioningCheck(ctx context.Context, r *postureRun, b *string) error {
	out, err := s.client.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: b})
	if err != nil {
		if apiCode(err) == "NotImplemented" {
			r.add(model.CheckVersioning, model.PostureWarn, "the provider does not version objects: an overwrite outside the platform cannot be undone")
			return nil
		}
		if r.skip(model.CheckVersioning, err, "") {
			return nil
		}
		return classify("get bucket versioning", err)
	}
	switch out.Status {
	case types.BucketVersioningStatusEnabled:
		r.add(model.CheckVersioning, model.PosturePass, "versioning is on: snapshots pin exact object versions")
	case types.BucketVersioningStatusSuspended:
		r.add(model.CheckVersioning, model.PostureWarn, "versioning is suspended: new writes replace objects in place; turn it back on")
	default:
		r.add(model.CheckVersioning, model.PostureWarn, "versioning is off: turn it on so an overwrite or deletion outside the platform can be undone")
	}
	return nil
}

func (s *ObjectStorage) objectLockCheck(ctx context.Context, r *postureRun, b *string) error {
	out, err := s.client.GetObjectLockConfiguration(ctx, &s3.GetObjectLockConfigurationInput{Bucket: b})
	switch {
	case apiCode(err) == "ObjectLockConfigurationNotFoundError":
		r.add(model.CheckObjectLock, model.PostureWarn, "Object Lock is off: retention and legal holds are kept by the platform only, not by the bucket")
		return nil
	case err != nil:
		if r.skip(model.CheckObjectLock, err, "") {
			return nil
		}
		return classify("get object lock configuration", err)
	}
	c := out.ObjectLockConfiguration
	if c == nil || c.ObjectLockEnabled != types.ObjectLockEnabledEnabled {
		r.add(model.CheckObjectLock, model.PostureWarn, "Object Lock is off: retention and legal holds are kept by the platform only, not by the bucket")
		return nil
	}
	detail := "Object Lock is on"
	if c.Rule != nil && c.Rule.DefaultRetention != nil {
		d := c.Rule.DefaultRetention
		detail += fmt.Sprintf(", default %s retention", d.Mode)
	}
	r.add(model.CheckObjectLock, model.PosturePass, detail)
	return nil
}

func rulePrefix(rule types.LifecycleRule) (string, bool) {
	if rule.Filter != nil {
		if rule.Filter.Prefix != nil {
			return aws.ToString(rule.Filter.Prefix), true
		}
		if rule.Filter.And != nil {
			return aws.ToString(rule.Filter.And.Prefix), rule.Filter.And.Tags == nil
		}
		if rule.Filter.Tag != nil {
			return "", false
		}
	}
	// Older rules carry the prefix at the top level.
	return aws.ToString(rule.Prefix), true
}

// overlaps reports whether a rule on rulePrefix reaches objects under one
// of the platform's prefixes.
func overlaps(rulePrefix string, prefixes []string) (string, bool) {
	for _, p := range prefixes {
		p = strings.TrimSuffix(p, "/") + "/"
		if strings.HasPrefix(p, rulePrefix) || strings.HasPrefix(rulePrefix, p) {
			return p, true
		}
	}
	return "", false
}

func (s *ObjectStorage) lifecycle(ctx context.Context, r *postureRun, b *string, prefixes []string) error {
	out, err := s.client.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: b})
	switch {
	case apiCode(err) == "NoSuchLifecycleConfiguration":
		r.facts.LifecycleHash = "none"
		r.add(model.CheckLifecycle, model.PosturePass, "the bucket has no lifecycle rules")
		r.add(model.CheckIncompleteUploads, model.PostureWarn,
			"no rule aborts incomplete multipart uploads; the platform cleans up its own, but add one after 7 days as a backstop")
		return nil
	case err != nil:
		if r.skip(model.CheckLifecycle, err, "") {
			r.add(model.CheckIncompleteUploads, model.PostureNotChecked, "the lifecycle rules could not be read")
			return nil
		}
		return classify("get bucket lifecycle", err)
	}

	canonical, _ := json.Marshal(out.Rules)
	sum := sha256.Sum256(canonical)
	r.facts.LifecycleHash = hex.EncodeToString(sum[:])

	var fails, warns []string
	aborts := false
	for _, rule := range out.Rules {
		if rule.Status != types.ExpirationStatusEnabled {
			continue
		}
		prefix, prefixOnly := rulePrefix(rule)
		at, reaches := overlaps(prefix, prefixes)
		if !reaches {
			continue
		}
		name := aws.ToString(rule.ID)
		if name == "" {
			name = "(unnamed)"
		}
		if !prefixOnly {
			warns = append(warns, fmt.Sprintf("rule %s filters by tag under %s; check it never matches the platform's objects", name, at))
			continue
		}
		if e := rule.Expiration; e != nil && (e.Days != nil || e.Date != nil) {
			fails = append(fails, fmt.Sprintf("rule %s expires current objects under %s: published versions would be deleted outside the platform", name, at))
		}
		if n := rule.NoncurrentVersionExpiration; n != nil && n.NoncurrentDays != nil {
			fails = append(fails, fmt.Sprintf("rule %s deletes non-current versions under %s after %d days: snapshots pinned to them would break",
				name, at, aws.ToInt32(n.NoncurrentDays)))
		}
		for _, t := range rule.Transitions {
			if t.StorageClass == types.TransitionStorageClassGlacier || t.StorageClass == types.TransitionStorageClassDeepArchive {
				warns = append(warns, fmt.Sprintf("rule %s moves objects under %s to %s: reads fail until each is restored", name, at, t.StorageClass))
			}
		}
		if rule.AbortIncompleteMultipartUpload != nil {
			aborts = true
		}
	}
	switch {
	case len(fails) > 0:
		r.add(model.CheckLifecycle, model.PostureFail, strings.Join(append(fails, warns...), "; "))
	case len(warns) > 0:
		r.add(model.CheckLifecycle, model.PostureWarn, strings.Join(warns, "; "))
	default:
		r.add(model.CheckLifecycle, model.PosturePass, fmt.Sprintf("%d lifecycle rules; none expires or archives the platform's objects", len(out.Rules)))
	}
	if aborts {
		r.add(model.CheckIncompleteUploads, model.PosturePass, "a rule aborts incomplete multipart uploads")
	} else {
		r.add(model.CheckIncompleteUploads, model.PostureWarn,
			"no rule aborts incomplete multipart uploads; the platform cleans up its own, but add one after 7 days as a backstop")
	}
	return nil
}

// location asks the provider where the bucket is. S3 answers "" for
// us-east-1 and "EU" for the oldest Ireland buckets.
func (s *ObjectStorage) location(ctx context.Context, r *postureRun, b *string) {
	out, err := s.plainClient().GetBucketLocation(ctx, &s3.GetBucketLocationInput{Bucket: b})
	if err != nil {
		return
	}
	switch location := strings.ToLower(string(out.LocationConstraint)); location {
	case "":
		if s.aws {
			r.facts.Location, r.facts.LocationVerified = "us-east-1", true
		}
	case "eu":
		r.facts.Location, r.facts.LocationVerified = "eu-west-1", true
	default:
		r.facts.Location, r.facts.LocationVerified = location, true
	}
}

// credentialScope looks for credentials broader than one bucket needs:
// the account's root user, or the right to list every bucket.
func (s *ObjectStorage) credentialScope(ctx context.Context, r *postureRun) {
	var problems []string
	if s.aws {
		o := s.client.Options()
		id, err := sts.New(sts.Options{Region: o.Region, Credentials: o.Credentials, HTTPClient: o.HTTPClient}).
			GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
		if err == nil {
			r.facts.OwnerAccount = aws.ToString(id.Account)
			if strings.HasSuffix(aws.ToString(id.Arn), ":root") {
				problems = append(problems, "these are the account root user's keys; use an IAM user or role limited to this bucket")
			}
		}
	}
	if _, err := s.plainClient().ListBuckets(ctx, &s3.ListBucketsInput{MaxBuckets: aws.Int32(1)}); err == nil {
		problems = append(problems, "the credentials can list every bucket in the account; limit them to this bucket")
	}
	if len(problems) > 0 {
		status := model.PostureWarn
		if strings.Contains(problems[0], "root") {
			status = model.PostureFail
		}
		r.add(model.CheckCredentialScope, status, strings.Join(problems, "; "))
		return
	}
	r.add(model.CheckCredentialScope, model.PosturePass, "the credentials cannot list other buckets")
}

// bucketOwner checks the bucket belongs to the expected account, or, when
// none is pinned, whether it belongs to the credentials' own account.
func (s *ObjectStorage) bucketOwner(ctx context.Context, r *postureRun, b *string, expected string) {
	owned := func(account string) error {
		_, err := s.plainClient().HeadBucket(ctx, &s3.HeadBucketInput{Bucket: b, ExpectedBucketOwner: aws.String(account)})
		return err
	}
	switch {
	case expected != "":
		if err := owned(expected); err != nil {
			r.add(model.CheckBucketOwner, model.PostureFail, "the bucket does not belong to account "+expected+", which the connection is pinned to")
			return
		}
		r.add(model.CheckBucketOwner, model.PosturePass, "every request names account "+expected+" as the bucket's owner")
	case r.facts.OwnerAccount != "" && owned(r.facts.OwnerAccount) == nil:
		r.add(model.CheckBucketOwner, model.PostureWarn, "the owner is not pinned; pin account "+r.facts.OwnerAccount+
			" so a bucket re-created by someone else under this name is refused")
	default:
		r.add(model.CheckBucketOwner, model.PostureWarn,
			"the owner is not pinned and the bucket is in another account; enter its account ID to pin it")
	}
}

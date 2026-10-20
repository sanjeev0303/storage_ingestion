package dataset

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/correlation"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/jsonl"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
)

// Dataset manifests are JSON Lines, schema version 1: one member per line,
//
//	{"object_id":"<id>","version_id":"<asset id>","label":"cat","split":"train","metadata":{"annotator":"a1"}}
//	{"namespace":"photos","path":"2024/cat-17.jpg","version":3,"label":"cat"}
//
// Fields: object_id, or path with an optional namespace (the catalog's
// path rules apply: no "..", no absolute paths); optionally version_id or
// version to pin one version (otherwise the snapshot takes the current
// one); label, split (train, validation, test), metadata (flat strings),
// schema_version (1). Lines are read as a stream and must be valid UTF-8
// JSON objects; blank lines are skipped. Unknown fields are refused, and
// a tenant_id field is refused outright: references always resolve in
// the importing tenant.
type manifestLine struct {
	SchemaVersion *int              `json:"schema_version"`
	TenantID      json.RawMessage   `json:"tenant_id"`
	ObjectID      string            `json:"object_id"`
	Namespace     string            `json:"namespace"`
	Path          string            `json:"path"`
	VersionID     string            `json:"version_id"`
	Version       int               `json:"version"`
	Label         string            `json:"label"`
	Split         string            `json:"split"`
	Metadata      map[string]string `json:"metadata"`
}

// Line codes beyond the member's own.
const (
	lineTenantField   = "tenant_field_not_allowed"
	lineSchemaVersion = "unsupported_schema_version"
)

// decodeLine reads one manifest line into a member input.
func decodeLine(raw []byte) (MemberInput, string, string) {
	if !utf8.Valid(raw) {
		return MemberInput{}, model.LineInvalidUTF8, "the line is not valid UTF-8"
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var line manifestLine
	if err := dec.Decode(&line); err != nil {
		return MemberInput{}, model.LineInvalidJSON, clip(err.Error())
	}
	if dec.More() {
		return MemberInput{}, model.LineInvalidJSON, "a line holds one JSON object"
	}
	if line.TenantID != nil {
		return MemberInput{}, lineTenantField, "tenant_id is not allowed: members always belong to the importing organization"
	}
	if line.SchemaVersion != nil && *line.SchemaVersion != 1 {
		return MemberInput{}, lineSchemaVersion, "schema_version must be 1"
	}
	return MemberInput{ObjectID: line.ObjectID, Namespace: line.Namespace, Path: line.Path, VersionID: line.VersionID,
		Version: line.Version, Label: line.Label, Split: line.Split, Metadata: line.Metadata}, "", ""
}

// manifestLimits are the platform's manifest limits, lowered by the
// tenant's quota.
func (s *Service) manifestLimits(ctx context.Context, tenantID string) (maxBytes, maxEntries int64) {
	maxBytes, maxEntries = s.cfg.MaxManifestBytes, s.cfg.MaxManifestEntries
	if s.deps.Quotas == nil {
		return maxBytes, maxEntries
	}
	q, err := s.deps.Quotas.Cached(ctx, tenantID)
	if err != nil {
		return maxBytes, maxEntries
	}
	if q.MaxManifestBytes > 0 && q.MaxManifestBytes < maxBytes {
		maxBytes = q.MaxManifestBytes
	}
	if q.MaxManifestEntries > 0 && q.MaxManifestEntries < maxEntries {
		maxEntries = q.MaxManifestEntries
	}
	return maxBytes, maxEntries
}

// ImportInput describes a manifest import.
type ImportInput struct {
	IdempotencyKey  string
	DuplicatePolicy model.DuplicatePolicy
	// SHA256, when given, must match the manifest's bytes.
	SHA256 string
	// SizeBytes is a staged manifest's size.
	SizeBytes int64
}

func (s *Service) checkImport(in *ImportInput) error {
	if in.IdempotencyKey == "" || len(in.IdempotencyKey) > 128 || hasControl(in.IdempotencyKey) {
		return fmt.Errorf("%w: an idempotency key of 1-128 characters is required", ErrInvalidRequest)
	}
	if in.DuplicatePolicy == "" {
		in.DuplicatePolicy = model.DuplicateReject
	}
	if !in.DuplicatePolicy.Valid() {
		return fmt.Errorf("%w: duplicate_policy must be REJECT, SKIP or REPLACE", ErrInvalidRequest)
	}
	in.SHA256 = strings.ToLower(strings.TrimSpace(in.SHA256))
	if in.SHA256 != "" {
		if b, err := hex.DecodeString(in.SHA256); err != nil || len(b) != sha256.Size {
			return fmt.Errorf("%w: sha256 must be 64 hex digits", ErrInvalidRequest)
		}
	}
	return nil
}

// ImportInline queues a manifest sent in the request body. The body is
// read once, bounded, and kept with the import; the worker reads its
// lines. The same idempotency key with the same manifest returns the
// first import.
func (s *Service) ImportInline(ctx context.Context, p model.Principal, datasetID string, in ImportInput, body io.Reader) (model.ManifestImport, bool, error) {
	if err := s.authorize(ctx, p, model.PermDatasetsWrite, "dataset.manifest_import", "dataset", datasetID); err != nil {
		return model.ManifestImport{}, false, err
	}
	if err := s.checkImport(&in); err != nil {
		return model.ManifestImport{}, false, err
	}
	maxBytes, _ := s.manifestLimits(ctx, p.TenantID)
	limit := min(s.cfg.MaxInlineManifestBytes, maxBytes)
	raw, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return model.ManifestImport{}, false, fmt.Errorf("%w: reading the manifest failed", ErrInvalidRequest)
	}
	if int64(len(raw)) > limit {
		return model.ManifestImport{}, false, fmt.Errorf("%w: at most %d bytes inline; stage a larger manifest", ErrManifestTooLarge, limit)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return model.ManifestImport{}, false, fmt.Errorf("%w: the manifest is empty", ErrInvalidRequest)
	}
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	if in.SHA256 != "" && in.SHA256 != digest {
		return model.ManifestImport{}, false, ErrManifestChecksum
	}

	now := s.now()
	id := event.NewID()
	ev := event.NewImportQueued(p.TenantID, id, now, correlation.From(ctx))
	imp, created, err := s.deps.Repo.CreateImport(ctx, model.ManifestImport{
		ID: id, TenantID: p.TenantID, DatasetID: datasetID, State: model.ImportQueued, Source: model.ManifestSourceInline,
		IdempotencyKey: in.IdempotencyKey, ManifestBytes: int64(len(raw)), ManifestSHA256: digest,
		DuplicatePolicy: in.DuplicatePolicy, CreatedBy: p.UserID, CreatedAt: now,
	}, raw, s.cfg.MaxActiveImports, &ev)
	if err == nil && created {
		s.record(ctx, p, "dataset.manifest_import", "dataset", datasetID, map[string]any{
			"import_id": imp.ID, "source": model.ManifestSourceInline, "bytes": len(raw), "sha256": digest})
		s.count("dataset_imports_created", 1)
	}
	return imp, created, err
}

// StagedUpload is where a client uploads a staged manifest.
type StagedUpload struct {
	URL       string
	ExpiresAt time.Time
}

// stagedKey is built from trusted identifiers only.
func stagedKey(tenantID, datasetID, importID string) string {
	return "dataset-manifests/" + tenantID + "/" + datasetID + "/" + importID + ".jsonl"
}

// StageImport records an import whose manifest is too large for one
// request, and gives a presigned PUT for it. SubmitImport queues it once
// it is uploaded.
func (s *Service) StageImport(ctx context.Context, p model.Principal, datasetID string, in ImportInput) (model.ManifestImport, StagedUpload, bool, error) {
	if err := s.authorize(ctx, p, model.PermDatasetsWrite, "dataset.manifest_import", "dataset", datasetID); err != nil {
		return model.ManifestImport{}, StagedUpload{}, false, err
	}
	if err := s.checkImport(&in); err != nil {
		return model.ManifestImport{}, StagedUpload{}, false, err
	}
	maxBytes, _ := s.manifestLimits(ctx, p.TenantID)
	if in.SizeBytes <= 0 || in.SizeBytes > maxBytes {
		return model.ManifestImport{}, StagedUpload{}, false, fmt.Errorf("%w: size_bytes must be between 1 and %d", ErrManifestTooLarge, maxBytes)
	}
	now := s.now()
	id := event.NewID()
	imp, created, err := s.deps.Repo.CreateImport(ctx, model.ManifestImport{
		ID: id, TenantID: p.TenantID, DatasetID: datasetID, State: model.ImportAwaitingUpload, Source: model.ManifestSourceStaged,
		IdempotencyKey: in.IdempotencyKey, ManifestKey: stagedKey(p.TenantID, datasetID, id), ManifestBytes: in.SizeBytes,
		ManifestSHA256: in.SHA256, DuplicatePolicy: in.DuplicatePolicy, CreatedBy: p.UserID, CreatedAt: now,
	}, nil, s.cfg.MaxActiveImports, nil)
	if err != nil {
		return imp, StagedUpload{}, false, err
	}
	if imp.State != model.ImportAwaitingUpload {
		return imp, StagedUpload{}, created, nil
	}
	plan, err := s.deps.Manifests.CreateUploadPlan(ctx, interfaces.UploadTarget{
		Bucket: s.deps.ManifestBucket, ObjectKey: imp.ManifestKey, ContentType: "application/x-ndjson",
		SizeBytes: imp.ManifestBytes, ExpiresAt: now.Add(15 * time.Minute),
	})
	if err != nil {
		return imp, StagedUpload{}, created, fmt.Errorf("presign manifest upload: %w", err)
	}
	if created {
		s.record(ctx, p, "dataset.manifest_import", "dataset", datasetID, map[string]any{
			"import_id": imp.ID, "source": model.ManifestSourceStaged, "bytes": in.SizeBytes})
		s.count("dataset_imports_created", 1)
	}
	return imp, StagedUpload{URL: plan.UploadURL, ExpiresAt: plan.ExpiresAt}, created, nil
}

// SubmitImport queues a staged import once its manifest is in storage
// with the declared size.
func (s *Service) SubmitImport(ctx context.Context, p model.Principal, datasetID, importID string) (model.ManifestImport, error) {
	if err := s.authorize(ctx, p, model.PermDatasetsWrite, "dataset.manifest_import", "dataset", datasetID); err != nil {
		return model.ManifestImport{}, err
	}
	imp, err := s.importOf(ctx, p, datasetID, importID)
	if err != nil {
		return imp, err
	}
	if imp.Source != model.ManifestSourceStaged {
		return imp, fmt.Errorf("%w: an inline import needs no submission", model.ErrImportState)
	}
	switch imp.State {
	case model.ImportAwaitingUpload:
	case model.ImportQueued, model.ImportRunning, model.ImportCompleted:
		return imp, nil // submitted already
	default:
		return imp, model.ErrImportState
	}
	meta, err := s.deps.Manifests.HeadObject(ctx, s.deps.ManifestBucket, imp.ManifestKey)
	if errors.Is(err, interfaces.ErrObjectNotFound) {
		return imp, ErrManifestNotStaged
	}
	if err != nil {
		return imp, fmt.Errorf("check staged manifest: %w", err)
	}
	if meta.SizeBytes != imp.ManifestBytes {
		return imp, fmt.Errorf("%w: the uploaded manifest is %d bytes, not the %d declared", ErrInvalidRequest, meta.SizeBytes, imp.ManifestBytes)
	}
	now := s.now()
	imp, err = s.deps.Repo.SubmitImport(ctx, p.TenantID, importID, meta.ETag, meta.SizeBytes, now,
		event.NewImportQueued(p.TenantID, importID, now, correlation.From(ctx)))
	if err == nil {
		s.record(ctx, p, "dataset.manifest_submit", "dataset", datasetID, map[string]any{"import_id": importID, "bytes": meta.SizeBytes})
	}
	return imp, err
}

// importOf returns the caller's import of the dataset.
func (s *Service) importOf(ctx context.Context, p model.Principal, datasetID, importID string) (model.ManifestImport, error) {
	imp, err := s.deps.Repo.GetImport(ctx, p.TenantID, importID)
	if err != nil {
		return imp, err
	}
	if imp.DatasetID != datasetID {
		return model.ManifestImport{}, model.ErrImportNotFound
	}
	return imp, nil
}

func (s *Service) Import(ctx context.Context, p model.Principal, datasetID, importID string) (model.ManifestImport, error) {
	if err := s.authorize(ctx, p, model.PermDatasetsRead, "dataset.read", "manifest_import", importID); err != nil {
		return model.ManifestImport{}, err
	}
	return s.importOf(ctx, p, datasetID, importID)
}

// ImportErrors pages an import's rejected lines; after is a line number.
func (s *Service) ImportErrors(ctx context.Context, p model.Principal, datasetID, importID string, after int64, limit int) ([]model.ImportError, error) {
	if _, err := s.Import(ctx, p, datasetID, importID); err != nil {
		return nil, err
	}
	size, err := s.pageSize(limit)
	if err != nil {
		return nil, err
	}
	return s.deps.Repo.ListImportErrors(ctx, p.TenantID, importID, after, size)
}

// CancelImport stops an import: at once if no worker holds it, otherwise
// after the batch being written. Members already added stay.
func (s *Service) CancelImport(ctx context.Context, p model.Principal, datasetID, importID string) (model.ManifestImport, error) {
	if err := s.authorize(ctx, p, model.PermDatasetsWrite, "dataset.manifest_cancel", "manifest_import", importID); err != nil {
		return model.ManifestImport{}, err
	}
	if _, err := s.importOf(ctx, p, datasetID, importID); err != nil {
		return model.ManifestImport{}, err
	}
	imp, err := s.deps.Repo.CancelImport(ctx, p.TenantID, importID, s.now())
	if err == nil {
		s.record(ctx, p, "dataset.manifest_cancel", "manifest_import", importID, map[string]any{"state": string(imp.State)})
	}
	return imp, err
}

// ─── The worker's side ────────────────────────────────────────────────

// RunImport reads an import named by its event. The event's tenant must
// be the import's.
func (s *Service) RunImport(ctx context.Context, owner, tenantID, importID string) (string, error) {
	imports, err := s.deps.Repo.ClaimImports(ctx, owner, importID, s.now(), s.cfg.Lease, 1)
	if err != nil {
		return "", err
	}
	if len(imports) == 0 {
		return "not due: being read, finished or waiting to retry", nil
	}
	imp := imports[0]
	if imp.TenantID != tenantID {
		// Never act on another tenant's import because an event said so.
		_ = s.deps.Repo.RetryImport(ctx, imp.ID, owner, s.now(), "event tenant mismatch", s.now())
		return "", retry.Permanent(fmt.Errorf("the event's tenant does not own import %s", imp.ID))
	}
	return s.runImport(ctx, owner, imp), nil
}

// RunDueImports claims due imports, up to slots, and reads them side by
// side.
func (s *Service) RunDueImports(ctx context.Context, owner string, slots int) (int, error) {
	imports, err := s.deps.Repo.ClaimImports(ctx, owner, "", s.now(), s.cfg.Lease, slots)
	if err != nil {
		return 0, err
	}
	var wg sync.WaitGroup
	for _, imp := range imports {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.runImport(ctx, owner, imp)
		}()
	}
	wg.Wait()
	return len(imports), nil
}

// runImport reads an import from its checkpoint for at most the import
// budget. Failures are kept on the import: it is retried with backoff
// until its attempts run out, then fails. The note says what happened.
func (s *Service) runImport(ctx context.Context, owner string, imp model.ManifestImport) string {
	log := s.deps.Log.With("tenant_id", imp.TenantID, "dataset_id", imp.DatasetID, "import_id", imp.ID)
	finish := func(state model.ImportState, reason string) string {
		if err := s.deps.Repo.FinishImport(context.WithoutCancel(ctx), imp.ID, owner, state, reason, s.now()); err != nil {
			log.Error("finishing manifest import failed", "state", state, "error", retry.Sanitize(err))
			return "finish failed"
		}
		s.count("dataset_imports_"+strings.ToLower(string(state)), 1)
		log.Info("manifest import ended", "state", state, "reason", reason)
		return "import " + strings.ToLower(string(state))
	}
	switch {
	case imp.CancelRequested:
		return finish(model.ImportCancelled, "")
	case imp.Attempts > s.cfg.MaxAttempts:
		// Dead: retried without progress until the attempts ran out.
		s.count("dataset_import_dead", 1)
		return finish(model.ImportFailed, "attempts exhausted: "+imp.LastError)
	}

	note, err := s.readImport(ctx, owner, imp, log)
	switch {
	case err == nil:
		return note
	case errors.Is(err, model.ErrImportState):
		return "lost the import's lease"
	case errors.Is(err, model.ErrDatasetArchived), errors.Is(err, model.ErrDatasetNotFound):
		return finish(model.ImportCancelled, "the dataset is archived")
	case retry.IsPermanent(err):
		return finish(model.ImportFailed, retry.Sanitize(err))
	}
	s.count("dataset_import_retries", 1)
	next := s.now().Add(s.retry.Delay(imp.Attempts))
	if rerr := s.deps.Repo.RetryImport(context.WithoutCancel(ctx), imp.ID, owner, next, retry.Sanitize(err), s.now()); rerr != nil {
		log.Error("scheduling a manifest import retry failed", "error", retry.Sanitize(rerr))
	}
	log.Warn("manifest import will be retried", "attempt", imp.Attempts, "error", retry.Sanitize(err))
	return "retry scheduled"
}

// readImport opens the manifest at the checkpoint and stores it batch by
// batch.
func (s *Service) readImport(ctx context.Context, owner string, imp model.ManifestImport, log *slog.Logger) (string, error) {
	_, maxEntries := s.manifestLimits(ctx, imp.TenantID)
	body, err := s.openManifest(ctx, imp)
	if err != nil {
		return "", err
	}
	defer body.Close()

	started := time.Now()
	deadline := started.Add(s.cfg.ImportBudget)
	reader := jsonl.NewReader(body, s.cfg.MaxManifestLineBytes, imp.CheckpointLine, imp.CheckpointOffset)
	rejectedSoFar := imp.LinesRejected
	for {
		items, lines, done, err := s.readBatch(reader, maxEntries)
		if err != nil {
			return "", err
		}
		cp := interfaces.ImportCheckpoint{ImportID: imp.ID, Owner: owner, Lines: reader.Line(), Offset: reader.Offset()}
		if len(items) > 0 || lines > 0 {
			if err := s.resolve(ctx, imp.TenantID, items); err != nil {
				return "", err
			}
			keep, dropped := dedupe(items, imp.DuplicatePolicy)
			changes := make([]model.MemberChange, len(keep))
			for j, i := range keep {
				changes[j] = items[i].change
			}
			for i, it := range items {
				code, detail := it.code, it.detail
				if code == "" && dropped[i] && imp.DuplicatePolicy == model.DuplicateReject {
					code, detail = model.LineDuplicateInFile, "the object appears earlier in the manifest"
				}
				if code == "" {
					continue
				}
				cp.RejectedCount++
				if rejectedSoFar+int64(len(cp.Rejected)) < s.cfg.MaxImportErrors {
					cp.Rejected = append(cp.Rejected, model.ImportError{Line: it.change.Line, Code: code, Detail: detail})
				}
			}
			if _, err := s.deps.Repo.AddMembers(ctx, imp.TenantID, imp.DatasetID, changes, imp.DuplicatePolicy,
				"import:"+imp.ID, imp.CreatedBy, s.now(), &cp); err != nil {
				return "", err
			}
			rejectedSoFar += int64(len(cp.Rejected))
			s.count("dataset_import_lines", lines)
			s.count("dataset_import_lines_rejected", cp.RejectedCount)
		}
		if done {
			if err := s.deps.Repo.FinishImport(context.WithoutCancel(ctx), imp.ID, owner, model.ImportCompleted, "", s.now()); err != nil {
				return "", err
			}
			s.count("dataset_imports_completed", 1)
			log.Info("manifest import completed", "lines", reader.Line(), "duration_ms", time.Since(started).Milliseconds())
			return fmt.Sprintf("import completed at line %d", reader.Line()), nil
		}
		cancel, err := s.deps.Repo.ExtendImport(ctx, imp.ID, owner, s.now(), s.cfg.Lease)
		if err != nil {
			return "", err
		}
		if cancel {
			if err := s.deps.Repo.FinishImport(context.WithoutCancel(ctx), imp.ID, owner, model.ImportCancelled, "", s.now()); err != nil {
				return "", err
			}
			s.count("dataset_imports_cancelled", 1)
			return "import cancelled", nil
		}
		if time.Now().After(deadline) {
			// Let go, so the next poll resumes from the checkpoint and one
			// huge manifest cannot hold a worker.
			if err := s.deps.Repo.RetryImport(ctx, imp.ID, owner, s.now(), "", s.now()); err != nil {
				return "", err
			}
			return fmt.Sprintf("read to line %d; continues", reader.Line()), nil
		}
	}
}

// readBatch reads up to ImportBatch lines. Blank lines count as read but
// hold no member; done is set at the manifest's end.
func (s *Service) readBatch(reader *jsonl.Reader, maxEntries int64) (items []parsed, lines int64, done bool, err error) {
	for len(items) < s.cfg.ImportBatch {
		raw, tooLong, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return items, lines, true, nil
		}
		if err != nil {
			return nil, 0, false, fmt.Errorf("read manifest: %w", err)
		}
		lines++
		number := reader.Line()
		if number > maxEntries {
			return nil, 0, false, retry.Permanent(fmt.Errorf("the manifest has more than %d lines", maxEntries))
		}
		var it parsed
		switch {
		case tooLong:
			it.code, it.detail = model.LineTooLong, fmt.Sprintf("the line is longer than %d bytes", s.cfg.MaxManifestLineBytes)
		case len(bytes.TrimSpace(raw)) == 0:
			continue
		default:
			in, code, detail := decodeLine(bytes.TrimSpace(raw))
			if code != "" {
				it.code, it.detail = code, detail
			} else {
				it = parseMember(in)
			}
		}
		it.change.Line = number
		items = append(items, it)
	}
	return items, lines, false, nil
}

// openManifest opens an import's manifest from its checkpoint. A staged
// manifest with a declared checksum is hashed whole before its first line
// is stored, so a corrupt upload adds nothing.
func (s *Service) openManifest(ctx context.Context, imp model.ManifestImport) (io.ReadCloser, error) {
	if imp.Source == model.ManifestSourceInline {
		raw, err := s.deps.Repo.InlineManifest(ctx, imp.ID, imp.CheckpointOffset)
		if err != nil {
			return nil, err
		}
		return io.NopCloser(bytes.NewReader(raw)), nil
	}
	if imp.CheckpointOffset >= imp.ManifestBytes {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	cond := interfaces.ReadCondition{IfMatch: imp.ManifestETag}
	gone := func(err error) error {
		if errors.Is(err, interfaces.ErrObjectNotFound) || errors.Is(err, interfaces.ErrPreconditionFailed) {
			return retry.Permanent(errors.New("the staged manifest disappeared or was replaced"))
		}
		return fmt.Errorf("open staged manifest: %w", err)
	}
	if imp.CheckpointOffset == 0 && imp.ManifestSHA256 != "" {
		whole, err := s.deps.Manifests.OpenRange(ctx, s.deps.ManifestBucket, imp.ManifestKey, 0, imp.ManifestBytes-1, cond)
		if err != nil {
			return nil, gone(err)
		}
		h := sha256.New()
		_, err = io.Copy(h, whole)
		whole.Close()
		if err != nil {
			return nil, fmt.Errorf("hash staged manifest: %w", err)
		}
		if hex.EncodeToString(h.Sum(nil)) != imp.ManifestSHA256 {
			return nil, retry.Permanent(ErrManifestChecksum)
		}
	}
	body, err := s.deps.Manifests.OpenRange(ctx, s.deps.ManifestBucket, imp.ManifestKey, imp.CheckpointOffset, imp.ManifestBytes-1, cond)
	if err != nil {
		return nil, gone(err)
	}
	return body, nil
}

// Package connectors joins the platform to the tools around it
// (docs/connectors.md): chat shares that post expiring, sign-in-gated
// links; a Git LFS endpoint; records of imported comms-archive and
// forensic exports; and a read-only DICOMweb gateway over snapshots.
package connectors

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/dto/request"
	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/netguard"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/secretbox"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
	"github.com/sanjeev0303/object-storage-platform/internal/service/dataset"
	"github.com/sanjeev0303/object-storage-platform/internal/service/delivery"
	uploadservice "github.com/sanjeev0303/object-storage-platform/internal/service/upload"
)

type Deps struct {
	Store    *postgres.ConnectorsRepository
	Uploads  *uploadservice.Service
	Delivery *delivery.Service
	Datasets *dataset.Service
	Sealer   secretbox.Sealer
	Audit    interfaces.AuditRecorder
	// Client posts to chat services; it must refuse private addresses.
	Client *http.Client
	// ConsoleURL is where a chat message's link points.
	ConsoleURL string
	// ChatHosts overrides the hosts allowed for each kind, in tests.
	ChatHosts map[string][]string
	// VerifyWait is how long a Git LFS verify waits for a file's checks: 30
	// seconds by default, not at all when negative.
	VerifyWait time.Duration
	Log        *slog.Logger
}

type Service struct {
	deps Deps
	now  func() time.Time
}

func New(deps Deps) *Service {
	return &Service{deps: deps, now: func() time.Time { return time.Now().UTC() }}
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{model.ErrInvalidConn}, args...)...)
}

func (s *Service) record(ctx context.Context, p model.Principal, action, kind, id string, details map[string]any) {
	if s.deps.Audit == nil {
		return
	}
	e := audit.From(ctx, p, action, kind, id)
	e.Details = details
	s.deps.Audit.Record(ctx, e)
}

// ─── Chat shares ──────────────────────────────────────────────────────

// The only hosts a message goes to: the URL is a tenant's input.
var chatHosts = map[string][]string{
	"SLACK": {"hooks.slack.com"},
	"TEAMS": {"webhook.office.com", "logic.azure.com", "api.powerplatform.com"},
}

func integrationAAD(tenantID, id string) []byte {
	return []byte("chat-integration\x00" + tenantID + "\x00" + id)
}

func (s *Service) hostsFor(kind string) []string {
	if h, ok := s.deps.ChatHosts[kind]; ok {
		return h
	}
	return chatHosts[kind]
}

// AddIntegration records a Slack or Teams incoming webhook. The URL is
// sealed: it is a credential, and it never comes back.
func (s *Service) AddIntegration(ctx context.Context, p model.Principal, kind, name, rawURL string) (model.ChatIntegration, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermOrgManage, "chat.integration_add", "chat_integration", ""); err != nil {
		return model.ChatIntegration{}, err
	}
	kind, name = strings.ToUpper(strings.TrimSpace(kind)), strings.TrimSpace(name)
	if _, ok := chatHosts[kind]; !ok {
		return model.ChatIntegration{}, invalid("kind is SLACK or TEAMS")
	}
	if name == "" || len(name) > 100 {
		return model.ChatIntegration{}, invalid("a name of 1 to 100 characters")
	}
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" || (u.Scheme != "https" && !slices.Contains(s.hostsFor(kind), "*")) {
		return model.ChatIntegration{}, invalid("an https webhook URL")
	}
	if !slices.Contains(s.hostsFor(kind), "*") && !netguard.HostIn(u.Hostname(), s.hostsFor(kind)...) {
		return model.ChatIntegration{}, invalid("a %s webhook is on %s", strings.ToLower(kind), strings.Join(s.hostsFor(kind), " or "))
	}
	c := model.ChatIntegration{ID: event.NewID(), TenantID: p.TenantID, Kind: kind, Name: name, URLHost: u.Hostname(), CreatedBy: p.UserID, CreatedAt: s.now()}
	sealed, err := s.deps.Sealer.Seal([]byte(u.String()), integrationAAD(p.TenantID, c.ID))
	if err != nil {
		return c, err
	}
	c.KeyVersion, c.Nonce, c.Cipher = sealed.KeyVersion, sealed.Nonce, sealed.Ciphertext
	if err := s.deps.Store.AddIntegration(ctx, c); err != nil {
		return c, err
	}
	s.record(ctx, p, "chat.integration_add", "chat_integration", c.ID, map[string]any{"kind": kind, "name": name, "host": c.URLHost})
	return c, nil
}

func (s *Service) Integrations(ctx context.Context, p model.Principal) ([]model.ChatIntegration, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermOrgRead, "chat.read", "chat_integration", ""); err != nil {
		return nil, err
	}
	return s.deps.Store.Integrations(ctx, p.TenantID)
}

func (s *Service) DeleteIntegration(ctx context.Context, p model.Principal, id string) error {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermOrgManage, "chat.integration_delete", "chat_integration", id); err != nil {
		return err
	}
	if err := s.deps.Store.DeleteIntegration(ctx, p.TenantID, id); err != nil {
		return err
	}
	s.record(ctx, p, "chat.integration_delete", "chat_integration", id, nil)
	return nil
}

const (
	defaultShareLife = 24 * time.Hour
	maxShareLife     = 30 * 24 * time.Hour
)

// ShareToChat posts a link to a file in a channel. The link opens a page
// for signed-in members with the right to read the file, and stops working
// at its expiry, so a chat export holds only dead links.
func (s *Service) ShareToChat(ctx context.Context, p model.Principal, assetID, integrationID, note string, life time.Duration) (model.ChatShare, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermAssetsDownload, "chat.share", "asset", assetID); err != nil {
		return model.ChatShare{}, err
	}
	if p.Scope.NoLinks || p.Scope.Restricted {
		return model.ChatShare{}, model.ErrForbidden
	}
	note = strings.TrimSpace(note)
	if len(note) > 500 {
		return model.ChatShare{}, invalid("a note of at most 500 characters")
	}
	if life == 0 {
		life = defaultShareLife
	}
	if life < time.Minute || life > maxShareLife {
		return model.ChatShare{}, invalid("a share lasts from a minute to 30 days")
	}
	if s.deps.ConsoleURL == "" {
		return model.ChatShare{}, invalid("chat sharing needs CONSOLE_PUBLIC_URL set on the server")
	}
	_, path, status, err := s.deps.Store.AssetFacts(ctx, p.TenantID, assetID)
	if err != nil {
		return model.ChatShare{}, err
	}
	if status != string(model.AssetStatusAvailable) {
		return model.ChatShare{}, invalid("the file is not available")
	}
	in, err := s.deps.Store.Integration(ctx, p.TenantID, integrationID)
	if err != nil {
		return model.ChatShare{}, err
	}
	now := s.now()
	c := model.ChatShare{ID: event.NewID(), TenantID: p.TenantID, AssetID: assetID, IntegrationID: in.ID, Note: note, ExpiresAt: now.Add(life),
		CreatedBy: p.UserID, CreatedAt: now, Path: path}
	c.URL = strings.TrimRight(s.deps.ConsoleURL, "/") + "/app/share/" + c.ID
	if err := s.deps.Store.AddShare(ctx, c); err != nil {
		return c, err
	}
	if err := s.post(ctx, in, c); err != nil {
		// No message went out, so no share stands.
		_ = s.deps.Store.RemoveShare(ctx, p.TenantID, c.ID)
		return model.ChatShare{}, err
	}
	s.record(ctx, p, "chat.share", "asset", assetID, map[string]any{"share_id": c.ID, "integration": in.Name, "expires_at": c.ExpiresAt})
	return c, nil
}

func (s *Service) post(ctx context.Context, in model.ChatIntegration, c model.ChatShare) error {
	raw, err := s.deps.Sealer.Open(secretbox.Sealed{KeyVersion: in.KeyVersion, Nonce: in.Nonce, Ciphertext: in.Cipher}, integrationAAD(in.TenantID, in.ID))
	if err != nil {
		return fmt.Errorf("open chat webhook: %w", err)
	}
	name := c.Path[strings.LastIndex(c.Path, "/")+1:]
	text := fmt.Sprintf("%s\nSign in to open: %s\nThe link stops working on %s.", name, c.URL, c.ExpiresAt.Format("2 Jan 2006 15:04 MST"))
	if c.Note != "" {
		text = c.Note + "\n" + text
	}
	body, _ := json.Marshal(map[string]string{"text": text})
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, string(raw), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.deps.Client.Do(req)
	if err != nil {
		// The error names the webhook URL, which is a credential.
		s.deps.Log.Warn("chat post failed", "integration_id", in.ID, "host", in.URLHost)
		return model.ErrChatDelivery
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode/100 != 2 {
		s.deps.Log.Warn("chat post refused", "integration_id", in.ID, "host", in.URLHost, "status", resp.StatusCode)
		return model.ErrChatDelivery
	}
	return nil
}

func (s *Service) Shares(ctx context.Context, p model.Principal) ([]model.ChatShare, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermOrgRead, "chat.read", "chat_share", ""); err != nil {
		return nil, err
	}
	return s.deps.Store.Shares(ctx, p.TenantID, 200)
}

// Share is what the share page shows.
func (s *Service) Share(ctx context.Context, p model.Principal, id string) (model.ChatShare, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermAssetsRead, "chat.open", "chat_share", id); err != nil {
		return model.ChatShare{}, err
	}
	c, err := s.deps.Store.Share(ctx, p.TenantID, id)
	if err != nil {
		return c, err
	}
	if c.RevokedAt != nil || !s.now().Before(c.ExpiresAt) {
		return c, model.ErrChatExpired
	}
	return c, nil
}

// OpenShare issues the signed-in member a download of the shared file: a
// short link in their name, through the usual checks and audit.
func (s *Service) OpenShare(ctx context.Context, p model.Principal, id, requestID string) (delivery.DownloadURL, error) {
	c, err := s.Share(ctx, p, id)
	if err != nil {
		return delivery.DownloadURL{}, err
	}
	out, err := s.deps.Delivery.CreateDownloadURL(ctx, p, c.AssetID, delivery.URLRequest{TTL: 5 * time.Minute, RequestID: requestID})
	if err != nil {
		return out, err
	}
	_ = s.deps.Store.CountOpen(ctx, p.TenantID, id)
	s.record(ctx, p, "chat.open", "chat_share", id, map[string]any{"asset_id": c.AssetID})
	return out, nil
}

func (s *Service) RevokeShare(ctx context.Context, p model.Principal, id string) error {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermAssetsDownload, "chat.revoke", "chat_share", id); err != nil {
		return err
	}
	c, err := s.deps.Store.Share(ctx, p.TenantID, id)
	if err != nil {
		return err
	}
	// Who shared it may take it back; anyone else needs to manage the org.
	if c.CreatedBy != p.UserID && !p.Permissions.Has(model.PermOrgManage) {
		return model.ErrForbidden
	}
	now := s.now()
	c.RevokedAt = &now
	if err := s.deps.Store.RevokeShare(ctx, c); err != nil {
		return err
	}
	s.record(ctx, p, "chat.revoke", "chat_share", id, nil)
	return nil
}

// ─── Collections ──────────────────────────────────────────────────────

// RecordCollection records an imported export: what it said it held, and
// what the import found (the SDK reads the export, the platform verifies
// each file's SHA-256 again when it is uploaded).
func (s *Service) RecordCollection(ctx context.Context, p model.Principal, c model.CollectionImport) (model.CollectionImport, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermDatasetsWrite, "collection.import", "dataset", c.DatasetID); err != nil {
		return c, err
	}
	if _, err := s.deps.Datasets.Get(ctx, p, c.DatasetID); err != nil {
		return c, err
	}
	c.Format = strings.ToUpper(c.Format)
	formats := []string{model.CollectionCommsJSONL, model.CollectionRSMF, model.CollectionUFDR, model.CollectionManifest}
	switch {
	case !slices.Contains(formats, c.Format):
		return c, invalid("format is one of %s", strings.Join(formats, ", "))
	case c.Items < 0 || c.Verified < 0 || c.Verified > c.Items:
		return c, invalid("0 <= verified <= items")
	case len(c.Source) > 500 || len(c.Mismatches) > 1000 || len(c.Retention) > 100:
		return c, invalid("source is at most 500 characters, and at most 1000 mismatches are listed")
	}
	if c.Mismatches == nil {
		c.Mismatches = []string{}
	}
	c.ID, c.TenantID, c.CreatedBy, c.CreatedAt = event.NewID(), p.TenantID, p.UserID, s.now()
	if err := s.deps.Store.AddCollection(ctx, c); err != nil {
		return c, err
	}
	s.record(ctx, p, "collection.import", "dataset", c.DatasetID, map[string]any{"collection_id": c.ID, "format": c.Format, "source": c.Source,
		"export_sha256": c.ExportSHA256, "items": c.Items, "verified": c.Verified, "mismatches": len(c.Mismatches), "retention": c.Retention})
	return c, nil
}

func (s *Service) Collections(ctx context.Context, p model.Principal, datasetID string) ([]model.CollectionImport, error) {
	if _, err := s.deps.Datasets.Get(ctx, p, datasetID); err != nil {
		return nil, err
	}
	return s.deps.Store.Collections(ctx, p.TenantID, datasetID, 200)
}

// ─── Git LFS ──────────────────────────────────────────────────────────

// LFSObject is one entry of a batch request and answer.
type LFSObject struct {
	OID           string               `json:"oid"`
	Size          int64                `json:"size"`
	Authenticated bool                 `json:"authenticated,omitempty"`
	Actions       map[string]LFSAction `json:"actions,omitempty"`
	Error         *LFSError            `json:"error,omitempty"`
}

type LFSAction struct {
	Href      string            `json:"href"`
	Header    map[string]string `json:"header,omitempty"`
	ExpiresAt time.Time         `json:"expires_at"`
}

type LFSError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

const lfsMaxObject = 5 << 30

var lfsOID = regexp.MustCompile(`^[0-9a-f]{64}$`)

func lfsPath(oid string) string { return ".lfs/" + oid[:2] + "/" + oid }

func allowDataset(p model.Principal, dataset string) error {
	if !p.Scope.Allows(dataset) {
		return model.ErrForbidden
	}
	return nil
}

// LFSBatch answers Git LFS's batch request. Objects are stored in the
// dataset's namespace under .lfs/, by their SHA-256.
func (s *Service) LFSBatch(ctx context.Context, p model.Principal, dataset, operation, base, authorization, requestID string, objects []LFSObject) ([]LFSObject, error) {
	if err := allowDataset(p, dataset); err != nil {
		return nil, err
	}
	if operation != "upload" && operation != "download" {
		return nil, invalid("the operation is upload or download")
	}
	if len(objects) > 1000 {
		return nil, invalid("at most 1000 objects in a batch")
	}
	out := make([]LFSObject, len(objects))
	for i, o := range objects {
		out[i] = LFSObject{OID: o.OID, Size: o.Size, Authenticated: true}
		if !lfsOID.MatchString(o.OID) || o.Size < 0 {
			out[i].Error = &LFSError{Code: 422, Message: "an oid is a SHA-256 in hex, with a size"}
			continue
		}
		var err error
		if operation == "download" {
			err = s.lfsDownload(ctx, p, dataset, requestID, &out[i])
		} else {
			err = s.lfsUpload(ctx, p, dataset, base, authorization, &out[i])
		}
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Service) lfsDownload(ctx context.Context, p model.Principal, dataset, requestID string, o *LFSObject) error {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermAssetsDownload, "lfs.download", "asset", ""); err != nil {
		return err
	}
	id, status, size, found, err := s.deps.Store.AssetByHash(ctx, p.TenantID, dataset, o.OID)
	if err != nil {
		return err
	}
	switch {
	case !found:
		o.Error = &LFSError{Code: 404, Message: "no such object"}
		return nil
	case status != string(model.AssetStatusAvailable):
		o.Error = &LFSError{Code: 404, Message: "the object is " + strings.ToLower(status)}
		return nil
	}
	o.Size = size
	u, err := s.deps.Delivery.CreateDownloadURL(ctx, p, id, delivery.URLRequest{TTL: time.Hour, RequestID: requestID})
	if err != nil {
		return err
	}
	o.Actions = map[string]LFSAction{"download": {Href: u.URL, ExpiresAt: u.ExpiresAt}}
	return nil
}

func (s *Service) lfsUpload(ctx context.Context, p model.Principal, dataset, base, authorization string, o *LFSObject) error {
	_, _, _, found, err := s.deps.Store.AssetByHash(ctx, p.TenantID, dataset, o.OID)
	if err != nil {
		return err
	}
	if found {
		return nil // no actions: the client has nothing to send
	}
	// The basic transfer adapter is one PUT, and one PUT holds up to 5 GiB.
	if o.Size > lfsMaxObject {
		o.Error = &LFSError{Code: 422, Message: "larger than 5 GiB: send it with the SDK, which uploads in parts"}
		return nil
	}
	res, err := s.deps.Uploads.CreateFor(ctx, p, request.CreateUploadRequest{FileName: o.OID, ContentType: "application/octet-stream", SizeBytes: o.Size,
		ChecksumSHA256: o.OID, IdempotencyKey: "lfs-" + dataset + "-" + o.OID, Dataset: dataset, Path: lfsPath(o.OID)})
	if err != nil {
		return err
	}
	header := map[string]string{"Content-Type": "application/octet-stream"}
	for k, v := range res.Plan.Headers {
		header[k] = v
	}
	o.Actions = map[string]LFSAction{
		"upload": {Href: res.Plan.UploadURL, Header: header, ExpiresAt: res.Plan.ExpiresAt},
		// Git LFS sends an action's own headers and no others, so the verify
		// call carries the caller's credential back to the caller.
		"verify": {Href: base + "/verify?upload=" + url.QueryEscape(res.Upload.ID), Header: map[string]string{"Authorization": authorization}, ExpiresAt: res.Plan.ExpiresAt},
	}
	return nil
}

// LFSVerify completes an upload and waits a little for its checks, so a
// push fails where a file is rejected (malware, wrong hash).
func (s *Service) LFSVerify(ctx context.Context, p model.Principal, dataset, uploadID, oid string) error {
	if err := allowDataset(p, dataset); err != nil {
		return err
	}
	if _, err := s.deps.Uploads.CompleteFor(ctx, p, uploadID); err != nil {
		return err
	}
	wait := s.deps.VerifyWait
	if wait == 0 {
		wait = 30 * time.Second
	}
	deadline := s.now().Add(wait)
	for s.now().Before(deadline) {
		_, status, _, found, err := s.deps.Store.AssetByHash(ctx, p.TenantID, dataset, oid)
		if err != nil {
			return err
		}
		if found && status == string(model.AssetStatusAvailable) {
			return nil
		}
		if found && status == string(model.AssetStatusRejected) {
			return invalid("the object was rejected by its checks")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return nil // still processing: a later download says so
}

func (s *Service) requireLFSWrite(ctx context.Context, p model.Principal, dataset, id string) error {
	if err := allowDataset(p, dataset); err != nil {
		return err
	}
	return audit.Require(ctx, s.deps.Audit, p, model.PermUploadsWrite, "lfs.lock", "dataset", id)
}

// LFSLock takes a path's lock. taken is false when someone holds it.
func (s *Service) LFSLock(ctx context.Context, p model.Principal, dataset, path string) (model.LFSLock, bool, error) {
	if err := s.requireLFSWrite(ctx, p, dataset, path); err != nil {
		return model.LFSLock{}, false, err
	}
	path = strings.TrimSpace(path)
	if path == "" || len(path) > 1024 {
		return model.LFSLock{}, false, invalid("a path of 1 to 1024 characters")
	}
	l, taken, err := s.deps.Store.AddLock(ctx, model.LFSLock{ID: event.NewID(), TenantID: p.TenantID, Dataset: dataset, Path: path, OwnerID: p.UserID, LockedAt: s.now()})
	if err == nil && taken {
		s.record(ctx, p, "lfs.lock", "dataset", dataset, map[string]any{"path": path})
	}
	return l, taken, err
}

func (s *Service) LFSLocks(ctx context.Context, p model.Principal, dataset, path, id string) ([]model.LFSLock, error) {
	if err := allowDataset(p, dataset); err != nil {
		return nil, err
	}
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermAssetsRead, "lfs.locks", "dataset", dataset); err != nil {
		return nil, err
	}
	return s.deps.Store.Locks(ctx, p.TenantID, dataset, path, id, 1000)
}

// LFSUnlock releases a lock: its owner's, or anyone's with force.
func (s *Service) LFSUnlock(ctx context.Context, p model.Principal, dataset, id string, force bool) (model.LFSLock, error) {
	if err := s.requireLFSWrite(ctx, p, dataset, id); err != nil {
		return model.LFSLock{}, err
	}
	locks, err := s.deps.Store.Locks(ctx, p.TenantID, dataset, "", id, 1)
	if err != nil {
		return model.LFSLock{}, err
	}
	if len(locks) == 0 {
		return model.LFSLock{}, model.ErrChatNotFound
	}
	if locks[0].OwnerID != p.UserID && !force {
		return locks[0], model.ErrLockHeld
	}
	if err := s.deps.Store.RemoveLock(ctx, p.TenantID, dataset, id); err != nil {
		return locks[0], err
	}
	s.record(ctx, p, "lfs.unlock", "dataset", dataset, map[string]any{"path": locks[0].Path, "force": force})
	return locks[0], nil
}

// ─── DICOMweb ─────────────────────────────────────────────────────────

// DicomInstances reads a snapshot's index. A superseded snapshot answers
// as it does for training: read its successor.
func (s *Service) DicomInstances(ctx context.Context, p model.Principal, datasetID, versionID string, f model.DicomFilter) ([]model.DicomInstance, error) {
	v, err := s.deps.Datasets.Version(ctx, p, datasetID, versionID)
	if err != nil {
		return nil, err
	}
	if v.SupersededBy != "" {
		return nil, fmt.Errorf("%w (%s): read its successor %s", model.ErrVersionSuperseded, v.SupersededReason, v.SupersededBy)
	}
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermAssetsRead, "dicomweb.search", "dataset_version", versionID); err != nil {
		return nil, err
	}
	return s.deps.Store.DicomInstances(ctx, p.TenantID, versionID, f)
}

// DicomRead streams instances' bytes through the delivery checks: each
// read authorized, audited, counted and verified against its SHA-256.
func (s *Service) DicomRead(ctx context.Context, p model.Principal, instances []model.DicomInstance, requestID string, each func(delivery.ShardFile) error) error {
	const batch = 200
	for start := 0; start < len(instances); start += batch {
		ids := make([]string, 0, batch)
		for _, in := range instances[start:min(start+batch, len(instances))] {
			ids = append(ids, in.AssetID)
		}
		if err := s.deps.Delivery.StreamShard(ctx, p, ids, requestID, each); err != nil {
			return err
		}
	}
	return nil
}

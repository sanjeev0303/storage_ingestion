package evidence

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
)

// anchorBody is what an anchor signs: a chain's head, and the anchor
// before it, so anchors chain too.
type anchorBody struct {
	Type       string    `json:"type"`
	Chain      string    `json:"organization_id"`
	Seq        int64     `json:"seq"`
	Hash       string    `json:"hash"`
	AnchoredAt time.Time `json:"anchored_at"`
	PrevSeq    int64     `json:"previous_seq,omitempty"`
	PrevHash   string    `json:"previous_hash,omitempty"`
}

// AnchorAll anchors every organization whose chain moved since its last
// anchor and whose interval has passed. A chain that does not verify is
// not anchored: it raises an AUDIT_CHAIN_BROKEN alert instead.
// ponytail: each anchor re-verifies the chain from its last checkpoint;
// verify from the last anchor once chains grow to many millions.
func (s *Service) AnchorAll(ctx context.Context) (int, error) {
	tenants, err := s.deps.Store.Tenants(ctx)
	if err != nil {
		return 0, err
	}
	var n int
	var errs []error
	for _, st := range tenants {
		if ctx.Err() != nil {
			break
		}
		done, err := s.anchor(ctx, st, false)
		if err != nil {
			s.deps.Log.Warn("anchor audit chain", "tenant_id", st.TenantID, "error", retry.Sanitize(err))
			errs = append(errs, err)
		}
		if done {
			n++
		}
	}
	return n, errors.Join(errs...)
}

// AnchorNow anchors the caller's chain at once.
func (s *Service) AnchorNow(ctx context.Context, p model.Principal) (model.AuditAnchor, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermAuditRead, "audit.anchor", "organization", p.TenantID); err != nil {
		return model.AuditAnchor{}, err
	}
	st, err := s.deps.Store.Settings(ctx, p.TenantID)
	if err != nil {
		return model.AuditAnchor{}, err
	}
	if _, err := s.anchor(ctx, st, true); err != nil {
		return model.AuditAnchor{}, err
	}
	list, err := s.deps.Store.Anchors(ctx, p.TenantID, 1)
	if err != nil || len(list) == 0 {
		return model.AuditAnchor{}, err
	}
	return list[0], nil
}

func (s *Service) anchor(ctx context.Context, st model.EvidenceSettings, force bool) (bool, error) {
	chain := st.TenantID
	now := s.now()
	last, err := s.deps.Store.Anchors(ctx, chain, 1)
	if err != nil {
		return false, err
	}
	if !force && len(last) > 0 && now.Sub(last[0].CreatedAt) < time.Duration(st.AnchorEveryMinutes)*time.Minute {
		return false, nil
	}
	seq, hash, err := s.deps.Store.Head(ctx, chain)
	if err != nil || seq == 0 || (len(last) > 0 && last[0].Seq == seq) {
		return false, err
	}
	v, err := s.deps.Chains.Verify(ctx, chain)
	if err != nil {
		return false, err
	}
	if v.BrokenAt != 0 {
		if s.deps.Alerts != nil {
			_, _ = s.deps.Alerts.AddAlert(ctx, model.SecurityAlert{ID: newID(), TenantID: chain, Kind: model.AlertChainBroken,
				Detail:   fmt.Sprintf("the audit chain does not verify at entry %d: %s", v.BrokenAt, v.Problem),
				Metrics:  map[string]any{"broken_at": v.BrokenAt, "checked": v.Checked},
				DedupKey: "chain-broken:" + now.Format("2006-01-02"), CreatedAt: now})
		}
		return false, fmt.Errorf("the audit chain does not verify at entry %d", v.BrokenAt)
	}

	body := anchorBody{Type: "audit-anchor/v1", Chain: chain, Seq: seq, Hash: hex.EncodeToString(hash), AnchoredAt: now}
	if len(last) > 0 {
		body.PrevSeq, body.PrevHash = last[0].Seq, last[0].Hash
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return false, err
	}
	a := model.AuditAnchor{ID: newID(), Chain: chain, Seq: seq, Hash: body.Hash, Body: string(raw), CreatedAt: now}
	if s.deps.Signer.Key != nil {
		a.KeyID, a.Signature = s.deps.Signer.KeyID, base64.StdEncoding.EncodeToString(ed25519.Sign(s.deps.Signer.Key, raw))
	}
	if st.AnchorConnectionID != "" {
		if err := s.writeAnchor(ctx, st, &a); err != nil {
			return false, err
		}
	}
	if s.deps.TSA != "" {
		sum := sha256.Sum256(raw)
		token, err := s.timestamp(ctx, sum[:])
		if err != nil {
			s.deps.Log.Warn("time-stamp an anchor", "tenant_id", chain, "error", retry.Sanitize(err))
		} else {
			a.TSAToken = token
		}
	}
	if err := s.deps.Store.PutAnchor(ctx, a); err != nil {
		return false, err
	}
	s.deps.Log.Info("audit chain anchored", "tenant_id", chain, "seq", seq, "object_key", a.ObjectKey)
	return true, nil
}

// writeAnchor writes the signed anchor to the organization's anchor
// connection, beside its data, under a dot folder the drift scan skips,
// and locks it in COMPLIANCE mode where the bucket has Object Lock.
func (s *Service) writeAnchor(ctx context.Context, st model.EvidenceSettings, a *model.AuditAnchor) error {
	loc, err := s.deps.Router.Locate(ctx, st.TenantID, st.AnchorConnectionID)
	if err != nil {
		return fmt.Errorf("anchor connection: %w", err)
	}
	doc, err := json.Marshal(map[string]string{"body": a.Body, "signature": a.Signature, "key_id": a.KeyID, "algorithm": "Ed25519"})
	if err != nil {
		return err
	}
	key := fmt.Sprintf("%s/%s/.anchors/%020d.json", loc.ReadyPrefix, st.TenantID, a.Seq)
	meta, err := loc.Storage.WriteObject(ctx, loc.ReadyBucket, key, bytes.NewReader(doc), int64(len(doc)), "application/json")
	if err != nil {
		return fmt.Errorf("write anchor: %w", err)
	}
	a.ConnectionID, a.ObjectKey, a.ObjectVersion = st.AnchorConnectionID, key, meta.VersionID
	locker, ok := loc.Storage.(interfaces.ObjectLocker)
	if !ok || st.AnchorLockDays == 0 || meta.VersionID == "" {
		return nil
	}
	if enabled, err := locker.ObjectLockEnabled(ctx, loc.ReadyBucket); err != nil || !enabled {
		return nil
	}
	until := a.CreatedAt.AddDate(0, 0, st.AnchorLockDays)
	if err := locker.LockObjectVersion(ctx, loc.ReadyBucket, key, meta.VersionID,
		interfaces.VersionLock{Mode: model.LockCompliance, Until: until}); err != nil {
		return fmt.Errorf("lock anchor: %w", err)
	}
	a.LockedUntil = &until
	return nil
}

// AnchorCheck is what checking one anchor found.
type AnchorCheck struct {
	model.AuditAnchor
	// SignatureOK, ChainOK (the chain still has that hash at that seq) and
	// StoredOK (the copy in the bucket is the same document).
	SignatureOK bool   `json:"signature_ok"`
	ChainOK     bool   `json:"chain_ok"`
	StoredOK    *bool  `json:"stored_ok,omitempty"`
	Problem     string `json:"problem,omitempty"`
}

// Anchors lists the caller's anchors, newest first, each checked: its
// signature, the chain against it, and the copy outside the database.
func (s *Service) Anchors(ctx context.Context, p model.Principal, limit int) ([]AnchorCheck, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermAuditRead, "audit.anchors", "organization", p.TenantID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	list, err := s.deps.Store.Anchors(ctx, p.TenantID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]AnchorCheck, 0, len(list))
	for _, a := range list {
		c := AnchorCheck{AuditAnchor: a}
		c.SignatureOK = s.signatureOK(a)
		hash, found, err := s.deps.Store.HashAt(ctx, a.Chain, a.Seq)
		switch {
		case err != nil:
			return nil, err
		case !found:
			c.Problem = "the chain no longer holds this entry (purged by retention)"
		default:
			c.ChainOK = hex.EncodeToString(hash) == a.Hash
			if !c.ChainOK {
				c.Problem = "the chain's entry no longer matches the anchored hash"
			}
		}
		if a.ObjectKey != "" {
			ok := s.storedOK(ctx, p.TenantID, a)
			c.StoredOK = &ok
			if !ok && c.Problem == "" {
				c.Problem = "the anchor in the bucket is missing or differs"
			}
		}
		out = append(out, c)
	}
	return out, nil
}

func (s *Service) signatureOK(a model.AuditAnchor) bool {
	if s.deps.Signer.Key == nil || a.Signature == "" {
		return false
	}
	sig, err := base64.StdEncoding.DecodeString(a.Signature)
	return err == nil && ed25519.Verify(s.deps.Signer.Key.Public().(ed25519.PublicKey), []byte(a.Body), sig)
}

func (s *Service) storedOK(ctx context.Context, tenantID string, a model.AuditAnchor) bool {
	loc, err := s.deps.Router.Locate(ctx, tenantID, a.ConnectionID)
	if err != nil {
		return false
	}
	body, err := loc.Storage.OpenRange(ctx, loc.ReadyBucket, a.ObjectKey, 0, -1, interfaces.ReadCondition{VersionID: a.ObjectVersion})
	if err != nil {
		return false
	}
	defer body.Close()
	raw, err := io.ReadAll(io.LimitReader(body, 1<<20))
	if err != nil {
		return false
	}
	var doc map[string]string
	return json.Unmarshal(raw, &doc) == nil && doc["body"] == a.Body && doc["signature"] == a.Signature
}

// ─── RFC 3161 ─────────────────────────────────────────────────────────

var oidSHA256 = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1}

type algorithmIdentifier struct {
	Algorithm  asn1.ObjectIdentifier
	Parameters asn1.RawValue `asn1:"optional"`
}

type messageImprint struct {
	HashAlgorithm algorithmIdentifier
	HashedMessage []byte
}

type timeStampReq struct {
	Version        int
	MessageImprint messageImprint
	Nonce          *big.Int `asn1:"optional"`
	CertReq        bool     `asn1:"optional"`
}

type pkiStatusInfo struct {
	Status int
	Rest   asn1.RawValue `asn1:"optional"`
}

type timeStampResp struct {
	Status pkiStatusInfo
	Token  asn1.RawValue `asn1:"optional"`
}

// timestamp asks the TSA to time-stamp digest and returns its token (a
// CMS SignedData, checkable with `openssl ts -verify`).
func (s *Service) timestamp(ctx context.Context, digest []byte) ([]byte, error) {
	nonce, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 63))
	if err != nil {
		return nil, err
	}
	req, err := asn1.Marshal(timeStampReq{Version: 1, CertReq: true, Nonce: nonce, MessageImprint: messageImprint{
		HashAlgorithm: algorithmIdentifier{Algorithm: oidSHA256, Parameters: asn1.NullRawValue}, HashedMessage: digest}})
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, s.deps.TSA, bytes.NewReader(req))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/timestamp-query")
	resp, err := s.deps.Client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the TSA answered HTTP %d", resp.StatusCode)
	}
	var out timeStampResp
	if _, err := asn1.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("read the TSA's answer: %w", err)
	}
	// 0 granted, 1 granted with modifications.
	if out.Status.Status > 1 || len(out.Token.FullBytes) == 0 {
		return nil, fmt.Errorf("the TSA refused (status %d)", out.Status.Status)
	}
	if !bytes.Contains(out.Token.FullBytes, digest) {
		return nil, errors.New("the TSA's token does not carry the anchor's digest")
	}
	return out.Token.FullBytes, nil
}

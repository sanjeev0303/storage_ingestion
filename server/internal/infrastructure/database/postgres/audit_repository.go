package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// AuditRepository stores the security audit log: append-only (enforced by
// triggers), one hash chain per tenant plus the platform's.
type AuditRepository struct {
	db *DB
}

func NewAuditRepository(db *DB) *AuditRepository {
	return &AuditRepository{db: db}
}

const auditColumns = `id, chain, seq, COALESCE(tenant_id, ''), actor_type, actor_id, action, resource_type, resource_id,
	outcome, reason, correlation_id, source_ip, details, created_at, prev_hash, hash`

func scanAudit(row pgx.Row) (model.AuditEvent, error) {
	var e model.AuditEvent
	var details []byte
	err := row.Scan(&e.ID, &e.Chain, &e.Seq, &e.TenantID, &e.ActorType, &e.ActorID, &e.Action, &e.ResourceType,
		&e.ResourceID, &e.Outcome, &e.Reason, &e.CorrelationID, &e.SourceIP, &details, &e.CreatedAt, &e.PrevHash, &e.Hash)
	if err != nil {
		return e, err
	}
	e.Details, err = decodeDetails(details)
	return e, err
}

// decodeDetails keeps numbers as json.Number, so re-encoding them gives
// the digits that were hashed.
func decodeDetails(raw []byte) (map[string]any, error) {
	details := map[string]any{}
	if len(raw) == 0 {
		return details, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&details); err != nil {
		return nil, fmt.Errorf("decode audit details: %w", err)
	}
	return details, nil
}

// canonicalDetails is the details' JSON as hashed: encoded, decoded with
// json.Number and encoded again, which is also what verification does
// with the stored JSONB.
func canonicalDetails(details map[string]any) ([]byte, error) {
	if details == nil {
		details = map[string]any{}
	}
	raw, err := json.Marshal(details)
	if err != nil {
		return nil, fmt.Errorf("encode audit details: %w", err)
	}
	decoded, err := decodeDetails(raw)
	if err != nil {
		return nil, err
	}
	return json.Marshal(decoded)
}

// auditHash chains e to prev: SHA-256 over prev and every field of e.
func auditHash(prev []byte, e model.AuditEvent, details []byte) []byte {
	h := sha256.New()
	h.Write(prev)
	for _, field := range []string{
		e.ID, e.Chain, strconv.FormatInt(e.Seq, 10), e.TenantID, e.ActorType, e.ActorID, e.Action, e.ResourceType,
		e.ResourceID, e.Outcome, e.Reason, e.CorrelationID, e.SourceIP, e.CreatedAt.UTC().Format(time.RFC3339Nano),
	} {
		h.Write([]byte(field))
		h.Write([]byte{0x1f})
	}
	h.Write(details)
	return h.Sum(nil)
}

func chainOf(tenantID string) string {
	if tenantID == "" {
		return model.PlatformChain
	}
	return tenantID
}

// appendAudit adds e to its chain inside tx. It takes the chain's head
// lock, so callers append last in their transaction: every other lock is
// already held, which rules out lock-order cycles.
func appendAudit(ctx context.Context, tx pgx.Tx, e model.AuditEvent) (model.AuditEvent, error) {
	if e.ID == "" {
		e.ID = event.NewID()
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	// PostgreSQL keeps microseconds: hash what will be stored.
	e.CreatedAt = e.CreatedAt.UTC().Truncate(time.Microsecond)
	e.Chain = chainOf(e.TenantID)

	var head int64
	var prev []byte
	err := tx.QueryRow(ctx, `
		INSERT INTO audit_chain_heads (chain, seq, hash, updated_at) VALUES ($1, 0, ''::bytea, $2)
		ON CONFLICT (chain) DO UPDATE SET updated_at = EXCLUDED.updated_at
		RETURNING seq, hash
	`, e.Chain, e.CreatedAt).Scan(&head, &prev)
	if err != nil {
		return e, fmt.Errorf("lock audit chain: %w", err)
	}
	e.Seq = head + 1
	e.PrevHash = nil
	if head > 0 {
		e.PrevHash = prev
	}
	details, err := canonicalDetails(e.Details)
	if err != nil {
		return e, err
	}
	e.Hash = auditHash(e.PrevHash, e, details)

	_, err = tx.Exec(ctx, `
		INSERT INTO audit_events (id, chain, seq, tenant_id, actor_type, actor_id, action, resource_type, resource_id,
		                          outcome, reason, correlation_id, source_ip, details, created_at, prev_hash, hash)
		VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
	`, e.ID, e.Chain, e.Seq, e.TenantID, e.ActorType, e.ActorID, e.Action, e.ResourceType, e.ResourceID,
		e.Outcome, clipText(e.Reason), e.CorrelationID, e.SourceIP, details, e.CreatedAt, e.PrevHash, e.Hash)
	if err != nil {
		return e, fmt.Errorf("insert audit event: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE audit_chain_heads SET seq = $2, hash = $3 WHERE chain = $1`, e.Chain, e.Seq, e.Hash); err != nil {
		return e, fmt.Errorf("advance audit chain: %w", err)
	}
	return e, nil
}

// appendAudits appends several events in order.
func appendAudits(ctx context.Context, tx pgx.Tx, events []model.AuditEvent) error {
	for _, e := range events {
		if _, err := appendAudit(ctx, tx, e); err != nil {
			return err
		}
	}
	return nil
}

func (r *AuditRepository) Append(ctx context.Context, e model.AuditEvent) (model.AuditEvent, error) {
	var stored model.AuditEvent
	err := withTx(ctx, r.db, func(tx pgx.Tx) error {
		var err error
		stored, err = appendAudit(ctx, tx, e)
		return err
	})
	return stored, err
}

func (r *AuditRepository) List(ctx context.Context, f interfaces.AuditFilter) ([]model.AuditEvent, error) {
	if f.Limit <= 0 || f.Limit > 1000 {
		f.Limit = 100
	}
	prefix := ""
	exact := f.Action
	if strings.HasSuffix(f.Action, ".") {
		prefix, exact = f.Action, ""
	}
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+auditColumns+` FROM audit_events
		WHERE chain = $1 AND ($2 = 0 OR seq < $2)
		  AND ($3 = '' OR action = $3) AND ($4 = '' OR starts_with(action, $4))
		  AND ($5 = '' OR outcome = $5) AND ($6 = '' OR actor_id = $6)
		ORDER BY seq DESC
		LIMIT $7
	`, f.Chain, f.BeforeSeq, exact, prefix, f.Outcome, f.ActorID, f.Limit)
	if err != nil {
		return nil, fmt.Errorf("list audit events: %w", err)
	}
	return collect(rows, scanAudit)
}

func (r *AuditRepository) Chains(ctx context.Context) ([]string, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT chain FROM audit_chain_heads ORDER BY chain`)
	if err != nil {
		return nil, fmt.Errorf("list audit chains: %w", err)
	}
	return collect(rows, func(row pgx.Row) (string, error) {
		var chain string
		return chain, row.Scan(&chain)
	})
}

func (r *AuditRepository) Verify(ctx context.Context, chain string) (interfaces.AuditVerification, error) {
	v := interfaces.AuditVerification{Chain: chain}
	var expected int64 = 1
	var prev []byte
	err := r.db.Pool.QueryRow(ctx, `
		SELECT seq, hash FROM audit_checkpoints WHERE chain = $1 ORDER BY seq DESC LIMIT 1
	`, chain).Scan(&expected, &prev)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		expected, prev = 1, nil
	case err != nil:
		return v, fmt.Errorf("read audit checkpoint: %w", err)
	default:
		expected++
	}

	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+auditColumns+` FROM audit_events WHERE chain = $1 AND seq >= $2 ORDER BY seq
	`, chain, expected)
	if err != nil {
		return v, fmt.Errorf("read audit chain: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		e, err := scanAudit(rows)
		if err != nil {
			return v, err
		}
		if v.FirstSeq == 0 {
			v.FirstSeq = e.Seq
		}
		v.Checked++
		v.LastSeq = e.Seq
		details, err := canonicalDetails(e.Details)
		if err != nil {
			return v, err
		}
		switch {
		case e.Seq != expected:
			v.BrokenAt, v.Problem = e.Seq, fmt.Sprintf("sequence gap: expected %d", expected)
		case !bytes.Equal(e.PrevHash, prev):
			v.BrokenAt, v.Problem = e.Seq, "previous hash does not match"
		case !bytes.Equal(auditHash(prev, e, details), e.Hash):
			v.BrokenAt, v.Problem = e.Seq, "content does not match its hash"
		}
		if v.BrokenAt != 0 {
			return v, nil
		}
		prev, expected = e.Hash, e.Seq+1
	}
	if err := rows.Err(); err != nil {
		return v, err
	}

	var headSeq int64
	var headHash []byte
	err = r.db.Pool.QueryRow(ctx, `SELECT seq, hash FROM audit_chain_heads WHERE chain = $1`, chain).Scan(&headSeq, &headHash)
	if err == nil && v.Checked > 0 && (headSeq != v.LastSeq || !bytes.Equal(headHash, prev)) {
		v.BrokenAt, v.Problem = v.LastSeq, "the chain head does not match the last event"
	}
	return v, nil
}

func (r *AuditRepository) Purge(ctx context.Context, before time.Time, limit int, now time.Time) (int, error) {
	chains, err := r.Chains(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, chain := range chains {
		if total >= limit {
			break
		}
		err := withTx(ctx, r.db, func(tx pgx.Tx) error {
			// The triggers refuse deletes unless this transaction says it
			// is the retention purge.
			if _, err := tx.Exec(ctx, `SET LOCAL storage.audit_purge = 'on'`); err != nil {
				return err
			}
			// Purge a prefix of the chain: from its first event up to the
			// newest expired one, at most the remaining limit. Sequence
			// numbers are contiguous, so the count is a subtraction.
			var first, newestOld *int64
			if err := tx.QueryRow(ctx, `
				SELECT min(seq), max(seq) FILTER (WHERE created_at < $2) FROM audit_events WHERE chain = $1
			`, chain, before).Scan(&first, &newestOld); err != nil {
				return fmt.Errorf("find purge boundary: %w", err)
			}
			if first == nil || newestOld == nil {
				return nil
			}
			last := min(*newestOld, *first+int64(limit-total)-1)
			var hash []byte
			if err := tx.QueryRow(ctx, `SELECT hash FROM audit_events WHERE chain = $1 AND seq = $2`, chain, last).Scan(&hash); err != nil {
				return fmt.Errorf("read purge boundary: %w", err)
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO audit_checkpoints (chain, seq, hash, created_at) VALUES ($1, $2, $3, $4)
				ON CONFLICT DO NOTHING
			`, chain, last, hash, now); err != nil {
				return fmt.Errorf("record audit checkpoint: %w", err)
			}
			tag, err := tx.Exec(ctx, `DELETE FROM audit_events WHERE chain = $1 AND seq <= $2`, chain, last)
			if err != nil {
				return fmt.Errorf("purge audit events: %w", err)
			}
			total += int(tag.RowsAffected())
			return nil
		})
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// withTx runs fn in a transaction on db.
func withTx(ctx context.Context, db *DB, fn func(pgx.Tx) error) error {
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

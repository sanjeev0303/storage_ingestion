package events

import (
	"context"
	"errors"
	"strings"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
)

// GuardedAdmin is dead-letter administration for the API. Sending an
// event again can redo work in any tenant, so every operation needs the
// platform's manage permission, and every attempt, allowed or not, is
// audited. The operator CLI (event-admin) uses Admin directly: it runs
// with database access, which is a platform operator's by definition.
type GuardedAdmin struct {
	admin *Admin
	dead  interfaces.DeadLetterRepository
	audit interfaces.AuditRecorder
}

func NewGuardedAdmin(admin *Admin, dead interfaces.DeadLetterRepository, recorder interfaces.AuditRecorder) *GuardedAdmin {
	return &GuardedAdmin{admin: admin, dead: dead, audit: recorder}
}

func (g *GuardedAdmin) require(ctx context.Context, p model.Principal, action, id string) error {
	if err := p.RequirePlatform(model.PlatformManage); err != nil {
		e := audit.Denied(ctx, p, action, "dead_letter", id, err.Error())
		e.TenantID = ""
		g.audit.Record(ctx, e)
		return err
	}
	return nil
}

func (g *GuardedAdmin) record(ctx context.Context, p model.Principal, action, id, reason string, details map[string]any) {
	e := audit.From(ctx, p, action, "dead_letter", id)
	e.TenantID, e.Reason, e.Details = "", reason, details
	g.audit.Record(ctx, e)
}

// List returns dead letters with status ("" for any), newest first.
func (g *GuardedAdmin) List(ctx context.Context, p model.Principal, status string, limit int) ([]model.DeadLetter, error) {
	if err := g.require(ctx, p, "dead_letter.list", ""); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	return g.dead.List(ctx, strings.ToUpper(status), limit)
}

func (g *GuardedAdmin) Inspect(ctx context.Context, p model.Principal, id string) (DeadLetterDetail, error) {
	if err := g.require(ctx, p, "dead_letter.inspect", id); err != nil {
		return DeadLetterDetail{}, err
	}
	return g.admin.Inspect(ctx, id)
}

// Retry sends the event again under its own identity.
func (g *GuardedAdmin) Retry(ctx context.Context, p model.Principal, id, reason string) error {
	if err := g.require(ctx, p, "dead_letter.retry", id); err != nil {
		return err
	}
	if err := validReason(reason); err != nil {
		return err
	}
	if err := g.admin.Retry(ctx, id, "user:"+p.UserID, reason); err != nil {
		return err
	}
	g.record(ctx, p, "dead_letter.retry", id, reason, nil)
	return nil
}

// Replay sends the event again under a new identity.
func (g *GuardedAdmin) Replay(ctx context.Context, p model.Principal, id, reason string) (string, error) {
	if err := g.require(ctx, p, "dead_letter.replay", id); err != nil {
		return "", err
	}
	if err := validReason(reason); err != nil {
		return "", err
	}
	replayID, err := g.admin.Replay(ctx, id, "user:"+p.UserID, reason)
	if err != nil {
		return "", err
	}
	g.record(ctx, p, "dead_letter.replay", id, reason, map[string]any{"replay_event_id": replayID})
	return replayID, nil
}

// Resolve closes a dead letter whose work was done another way; Discard
// closes one that is no longer wanted.
func (g *GuardedAdmin) Resolve(ctx context.Context, p model.Principal, id, reason string) error {
	return g.close(ctx, p, id, reason, "dead_letter.resolve", g.admin.Resolve)
}

func (g *GuardedAdmin) Discard(ctx context.Context, p model.Principal, id, reason string) error {
	return g.close(ctx, p, id, reason, "dead_letter.discard", g.admin.Discard)
}

func (g *GuardedAdmin) close(ctx context.Context, p model.Principal, id, reason, action string,
	fn func(ctx context.Context, id, actor, reason string) error) error {
	if err := g.require(ctx, p, action, id); err != nil {
		return err
	}
	if err := validReason(reason); err != nil {
		return err
	}
	if err := fn(ctx, id, "user:"+p.UserID, reason); err != nil {
		return err
	}
	g.record(ctx, p, action, id, reason, nil)
	return nil
}

// ErrReasonRequired: dead-letter actions are kept with a reason.
var ErrReasonRequired = errors.New("a reason of at most 500 characters is required")

func validReason(reason string) error {
	if strings.TrimSpace(reason) == "" || len(reason) > 500 {
		return ErrReasonRequired
	}
	return nil
}

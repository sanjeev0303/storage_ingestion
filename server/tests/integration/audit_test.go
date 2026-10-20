package integration

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/correlation"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
)

func TestAuditEventsAreRecordedAndAccessControlled(t *testing.T) {
	e := openSecurityEnv(t, nil)
	acme := e.team(t, "aud")
	globex := e.team(t, "glx")
	newcomer, _ := e.account(t, "newcomer")

	// A security change records who, what, from where and in which request.
	ctx := correlation.WithSource(correlation.With(context.Background(), "req-audit-1"), "198.51.100.7")
	inv, err := e.identity.Invite(ctx, acme.admin, newcomer.Email, model.RoleDeveloper)
	if err != nil {
		t.Fatal(err)
	}
	events, err := e.audit.List(ctx, acme.auditor, audit.Filter{Action: "invitation.create"})
	if err != nil {
		t.Fatal(err)
	}
	var created *model.AuditEvent
	for i := range events {
		if events[i].ResourceID == inv.ID {
			created = &events[i]
		}
	}
	if created == nil || created.ActorID != acme.adminUser.ID || created.ActorType != model.ActorUser ||
		created.Outcome != model.AuditSuccess || created.CorrelationID != "req-audit-1" || created.SourceIP != "198.51.100.7" ||
		created.Details["role"] != model.RoleDeveloper || len(created.Hash) != 32 {
		t.Fatalf("invitation event: %+v", created)
	}
	if strings.Contains(created.Reason+created.ResourceID, newcomer.Email) || created.Details["email"] != nil {
		t.Fatal("the audit record names the role, not the address")
	}

	// Reading the log needs audit.read, and both the read and a refusal
	// are recorded.
	if _, err := e.audit.List(ctx, acme.developer, audit.Filter{}); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("developer reads the audit log: %v", err)
	}
	reads := e.auditEvents(t, acme.org.ID, "audit.read")
	outcomes := map[string]int{}
	for _, r := range reads {
		outcomes[r.Outcome]++
	}
	if outcomes[model.AuditSuccess] != 1 || outcomes[model.AuditDenied] != 1 {
		t.Fatalf("audit reads: %v", outcomes)
	}

	// Each organization sees only its own log.
	theirs, err := e.audit.List(ctx, globex.auditor, audit.Filter{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range theirs {
		if ev.TenantID != globex.org.ID {
			t.Fatalf("globex's auditor saw %s's event %s", ev.TenantID, ev.Action)
		}
	}
	if _, err := e.audit.Verify(ctx, globex.auditor, acme.org.ID); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("verifying another organization's chain: %v", err)
	}
	// The platform log is for platform administrators and auditors.
	if _, err := e.audit.PlatformList(ctx, acme.owner, model.PlatformChain, audit.Filter{}); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("an owner reads the platform log: %v", err)
	}
	platformAuditor := model.Principal{UserID: "it-platform-auditor", PlatformRole: model.PlatformAuditor}
	if _, err := e.audit.PlatformList(ctx, platformAuditor, acme.org.ID, audit.Filter{Limit: 1}); err != nil {
		t.Fatalf("a platform auditor reads any chain: %v", err)
	}
}

func TestAuditLogIsAppendOnlyAndTamperEvident(t *testing.T) {
	e := openSecurityEnv(t, nil)
	ctx := context.Background()
	acme := e.team(t, "chain")
	chain := acme.org.ID

	v, err := e.audit.Verify(ctx, acme.auditor, chain)
	if err != nil || v.BrokenAt != 0 || v.Checked < 4 || v.FirstSeq != 1 {
		t.Fatalf("a fresh chain verifies: %+v %v", v, err)
	}

	// Ordinary statements cannot change or remove events.
	var victim model.AuditEvent
	if err := e.db.Pool.QueryRow(ctx, `SELECT id, seq, reason FROM audit_events WHERE chain = $1 AND seq = 2`, chain).
		Scan(&victim.ID, &victim.Seq, &victim.Reason); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`UPDATE audit_events SET reason = 'edited' WHERE id = $1`,
		`DELETE FROM audit_events WHERE id = $1`,
	} {
		if _, err := e.db.Pool.Exec(ctx, q, victim.ID); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Fatalf("%s: expected the append-only refusal, got %v", q, err)
		}
	}
	// TRUNCATE too; the transaction is rolled back whatever happens.
	tx, err := e.db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(ctx, `TRUNCATE audit_events`)
	_ = tx.Rollback(ctx)
	if err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("TRUNCATE: %v", err)
	}

	// Someone with enough database access to bypass the triggers still
	// cannot rewrite history unnoticed: a forged event breaks the chain.
	copyTable := "audit_tamper_" + e.run
	if _, err := e.db.Pool.Exec(ctx, `CREATE TABLE `+copyTable+` AS SELECT * FROM audit_events WHERE id = $1`, victim.ID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = e.db.Pool.Exec(context.Background(), `DROP TABLE IF EXISTS `+copyTable) })
	replace := func(reason string) {
		t.Helper()
		if _, err := e.db.Pool.Exec(ctx, `UPDATE `+copyTable+` SET reason = $1`, reason); err != nil {
			t.Fatal(err)
		}
		err := withPurge(ctx, e.db, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `DELETE FROM audit_events WHERE id = $1`, victim.ID); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO audit_events SELECT * FROM `+copyTable)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	replace("forged")
	if v, err := e.auditRepo.Verify(ctx, chain); err != nil || v.BrokenAt != victim.Seq || !strings.Contains(v.Problem, "content") {
		t.Fatalf("a forged event must be found: %+v %v", v, err)
	}
	replace(victim.Reason)
	if v, err := e.auditRepo.Verify(ctx, chain); err != nil || v.BrokenAt != 0 {
		t.Fatalf("restored: %+v %v", v, err)
	}

	// So does a removed one.
	err = withPurge(ctx, e.db, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM audit_events WHERE id = $1`, victim.ID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if v, err := e.auditRepo.Verify(ctx, chain); err != nil || v.BrokenAt != victim.Seq+1 || !strings.Contains(v.Problem, "gap") {
		t.Fatalf("a removed event must be found: %+v %v", v, err)
	}
	replace(victim.Reason)
}

func TestAuditRetentionPurgeKeepsTheChainVerifiable(t *testing.T) {
	e := openSecurityEnv(t, nil)
	ctx := context.Background()
	acme := e.team(t, "purge")
	chain := acme.org.ID

	// The service purges only past AUDIT_RETENTION (at least a day), so
	// a fresh chain keeps every event.
	if _, err := e.audit.Purge(ctx, 100000); err != nil {
		t.Fatal(err)
	}
	if v, err := e.auditRepo.Verify(ctx, chain); err != nil || v.FirstSeq != 1 || v.BrokenAt != 0 {
		t.Fatalf("retention purged fresh events: %+v %v", v, err)
	}

	// The repository purges a prefix of each chain older than a cut-off,
	// leaving a checkpoint so the rest still verifies. (This purges every
	// chain in the test database up to the cut-off, as retention would.)
	var cut time.Time
	if err := e.db.Pool.QueryRow(ctx, `SELECT created_at FROM audit_events WHERE chain = $1 AND seq = 2`, chain).Scan(&cut); err != nil {
		t.Fatal(err)
	}
	if _, err := e.auditRepo.Purge(ctx, cut.Add(time.Microsecond), 100000, time.Now()); err != nil {
		t.Fatal(err)
	}
	v, err := e.auditRepo.Verify(ctx, chain)
	if err != nil || v.BrokenAt != 0 || v.FirstSeq != 3 {
		t.Fatalf("after purging two events: %+v %v", v, err)
	}
	for _, c := range []string{model.PlatformChain, chain} {
		if v, err := e.auditRepo.Verify(ctx, c); err != nil || v.BrokenAt != 0 {
			t.Fatalf("chain %s after the purge: %+v %v", c, v, err)
		}
	}
	// New events keep extending the purged chain.
	if _, err := e.identity.Invite(ctx, acme.admin, e.email("later"), model.RoleAuditor); err != nil {
		t.Fatal(err)
	}
	if v, err := e.auditRepo.Verify(ctx, chain); err != nil || v.BrokenAt != 0 || v.LastSeq <= 3 {
		t.Fatalf("appending after a purge: %+v %v", v, err)
	}
}

func TestProtectedChangesCommitOrRollBackWithTheirAuditEvent(t *testing.T) {
	e := openSecurityEnv(t, nil)
	ctx := context.Background()
	acme := e.team(t, "atomic")
	tokens := e.login(t, acme.devUser.Email, acme.devPW)
	head := func() int64 {
		var seq int64
		if err := e.db.Pool.QueryRow(ctx, `SELECT seq FROM audit_chain_heads WHERE chain = $1`, acme.org.ID).Scan(&seq); err != nil {
			t.Fatal(err)
		}
		return seq
	}
	// An event the audit table refuses (an unknown actor type) makes the
	// whole change fail.
	unrecordable := model.AuditEvent{
		TenantID: acme.org.ID, ActorType: "ROBOT", ActorID: "x", Action: "test", ResourceType: "test", Outcome: model.AuditSuccess,
	}
	before := head()

	_, err := e.tenancy.ChangeMember(ctx, interfaces.MemberChange{
		OrgID: acme.org.ID, UserID: acme.devUser.ID, RoleID: model.RoleAuditor, Now: time.Now().UTC(),
	}, func(model.Membership, *model.Membership) model.AuditEvent { return unrecordable })
	if err == nil {
		t.Fatal("a role change whose audit event cannot be written must fail")
	}
	if p := e.as(t, acme.org.ID, acme.devUser); p.RoleID != model.RoleDeveloper {
		t.Fatalf("the role change was rolled back: %+v", p)
	}

	platformEvent := unrecordable
	platformEvent.TenantID = ""
	if _, err := e.users.SetUserStatus(ctx, acme.devUser.ID, model.UserDeactivated, time.Now().UTC(), platformEvent); err == nil {
		t.Fatal("a deactivation whose audit event cannot be written must fail")
	}
	if _, err := e.identity.Authenticate(ctx, tokens.AccessToken); err != nil {
		t.Fatalf("the deactivation and its session revocation were rolled back: %v", err)
	}

	quotaBefore, err := e.quotaRepo.Policy(ctx, acme.org.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.quotaRepo.SetPolicy(ctx, acme.org.ID, model.TenantQuota{MaxActiveUploads: 1}, time.Now().UTC(), unrecordable); err == nil {
		t.Fatal("a quota change whose audit event cannot be written must fail")
	}
	if after, _ := e.quotaRepo.Policy(ctx, acme.org.ID); after != quotaBefore {
		t.Fatalf("the quota change was rolled back: %+v", after)
	}
	if head() != before {
		t.Fatal("no event of a rolled-back change may remain")
	}

	// The other way round: a refused change writes no success event.
	if _, err := e.identity.ChangeMemberRole(ctx, acme.owner, acme.ownerUser.ID, model.RoleAdmin); !errors.Is(err, model.ErrLastOwner) {
		t.Fatal(err)
	}
	for _, ev := range e.auditEvents(t, acme.org.ID, "member.role_change") {
		if ev.Outcome == model.AuditSuccess {
			t.Fatalf("a refused change recorded success: %+v", ev)
		}
	}
	if head() != before+1 {
		t.Fatalf("the refusal itself is recorded once: head %d, was %d", head(), before)
	}
}

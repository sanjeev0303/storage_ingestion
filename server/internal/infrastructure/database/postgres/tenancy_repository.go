package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// TenancyRepository stores organizations, memberships, custom roles and
// invitations. Membership changes take the organization's row lock, so
// two concurrent demotions cannot both pass the last-owner check.
type TenancyRepository struct {
	db *DB
}

func NewTenancyRepository(db *DB) *TenancyRepository {
	return &TenancyRepository{db: db}
}

const orgColumns = `o.id, o.name, o.slug, o.status, o.settings, o.created_by, o.created_at, o.updated_at, o.deactivated_at`

func scanOrg(row pgx.Row) (model.Organization, error) {
	var o model.Organization
	var status string
	var settings []byte
	err := row.Scan(&o.ID, &o.Name, &o.Slug, &status, &settings, &o.CreatedBy, &o.CreatedAt, &o.UpdatedAt, &o.DeactivatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, model.ErrOrgNotFound
	}
	if err != nil {
		return o, err
	}
	o.Status = model.OrgStatus(status)
	if err := json.Unmarshal(settings, &o.Settings); err != nil {
		return o, fmt.Errorf("decode organization settings: %w", err)
	}
	return o, nil
}

const memberColumns = `m.org_id, m.user_id, m.role_id, m.status, m.created_by, m.created_at, m.updated_at, u.email, u.display_name`

func scanMember(row pgx.Row) (model.Membership, error) {
	var m model.Membership
	var status string
	err := row.Scan(&m.OrgID, &m.UserID, &m.RoleID, &status, &m.CreatedBy, &m.CreatedAt, &m.UpdatedAt, &m.Email, &m.DisplayName)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, model.ErrMembershipNotFound
	}
	m.Status = model.MembershipStatus(status)
	return m, err
}

const roleColumns = `r.id, r.org_id, r.name, r.description, r.permissions, r.created_by, r.created_at, r.updated_at`

func scanRole(row pgx.Row) (model.Role, error) {
	var r model.Role
	var names []string
	err := row.Scan(&r.ID, &r.OrgID, &r.Name, &r.Description, &names, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, model.ErrRoleNotFound
	}
	if err != nil {
		return r, err
	}
	// A permission that no longer exists is dropped rather than failing
	// every request of the role's members.
	for _, name := range names {
		if p, err := model.ParsePermissions([]string{name}); err == nil {
			r.Permissions |= p
		}
	}
	return r, nil
}

const invitationColumns = `i.id, i.org_id, i.email, i.role_id, i.status, i.created_by, i.created_at, i.expires_at,
	i.accepted_at, COALESCE(i.accepted_by, '')`

func scanInvitation(row pgx.Row) (model.Invitation, error) {
	var inv model.Invitation
	var status string
	err := row.Scan(&inv.ID, &inv.OrgID, &inv.Email, &inv.RoleID, &status, &inv.CreatedBy, &inv.CreatedAt,
		&inv.ExpiresAt, &inv.AcceptedAt, &inv.AcceptedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return inv, model.ErrInvitationNotFound
	}
	inv.Status = model.InvitationStatus(status)
	return inv, err
}

func insertMember(ctx context.Context, tx pgx.Tx, m model.Membership) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO memberships (org_id, user_id, role_id, status, created_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $6)
	`, m.OrgID, m.UserID, m.RoleID, string(m.Status), m.CreatedBy, m.CreatedAt)
	if isConstraint(err, "memberships_pkey") {
		return model.ErrMembershipExists
	}
	if err != nil {
		return fmt.Errorf("add member: %w", err)
	}
	return nil
}

// ─── Organizations ────────────────────────────────────────────────────

func (r *TenancyRepository) CreateOrganization(ctx context.Context, org model.Organization, owner model.Membership, audit ...model.AuditEvent) error {
	settings, err := json.Marshal(org.Settings)
	if err != nil {
		return err
	}
	return withTx(ctx, r.db, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO organizations (id, name, slug, status, settings, created_by, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $7)
		`, org.ID, org.Name, org.Slug, string(org.Status), settings, org.CreatedBy, org.CreatedAt)
		if isConstraint(err, "ux_organizations_slug") {
			return model.ErrOrgExists
		}
		if err != nil {
			return fmt.Errorf("create organization: %w", err)
		}
		if err := insertMember(ctx, tx, owner); err != nil {
			return err
		}
		return appendAudits(ctx, tx, audit)
	})
}

func (r *TenancyRepository) GetOrganization(ctx context.Context, orgID string) (model.Organization, error) {
	return scanOrg(r.db.Pool.QueryRow(ctx, `SELECT `+orgColumns+` FROM organizations AS o WHERE o.id = $1`, orgID))
}

func (r *TenancyRepository) ListOrganizations(ctx context.Context, after string, limit int) ([]model.Organization, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT `+orgColumns+` FROM organizations AS o WHERE o.id > $1 ORDER BY o.id LIMIT $2`, after, limit)
	if err != nil {
		return nil, fmt.Errorf("list organizations: %w", err)
	}
	return collect(rows, scanOrg)
}

func (r *TenancyRepository) UpdateOrganization(ctx context.Context, org model.Organization, audit ...model.AuditEvent) (model.Organization, error) {
	settings, err := json.Marshal(org.Settings)
	if err != nil {
		return org, err
	}
	var out model.Organization
	err = withTx(ctx, r.db, func(tx pgx.Tx) error {
		var err error
		out, err = scanOrg(tx.QueryRow(ctx, `
			UPDATE organizations AS o SET name = $2, settings = $3, updated_at = $4
			WHERE o.id = $1 AND o.status IN ('ACTIVE', 'SUSPENDED')
			RETURNING `+orgColumns, org.ID, org.Name, settings, org.UpdatedAt))
		if errors.Is(err, model.ErrOrgNotFound) {
			return model.ErrOrgState
		}
		if err != nil {
			return err
		}
		return appendAudits(ctx, tx, audit)
	})
	return out, err
}

func (r *TenancyRepository) SetOrganizationStatus(ctx context.Context, orgID string, from []model.OrgStatus, to model.OrgStatus, now time.Time, audit ...model.AuditEvent) (model.Organization, error) {
	allowed := make([]string, len(from))
	for i, s := range from {
		allowed[i] = string(s)
	}
	var out model.Organization
	err := withTx(ctx, r.db, func(tx pgx.Tx) error {
		var err error
		out, err = scanOrg(tx.QueryRow(ctx, `
			UPDATE organizations AS o
			SET status = $2, updated_at = $3,
			    deactivated_at = CASE WHEN $2 IN ('DEACTIVATED', 'DELETED') THEN COALESCE(o.deactivated_at, $3) ELSE NULL END
			WHERE o.id = $1 AND o.status = ANY($4)
			RETURNING `+orgColumns, orgID, string(to), now, allowed))
		if errors.Is(err, model.ErrOrgNotFound) {
			if _, getErr := scanOrg(tx.QueryRow(ctx, `SELECT `+orgColumns+` FROM organizations AS o WHERE o.id = $1`, orgID)); getErr != nil {
				return getErr
			}
			return model.ErrOrgState
		}
		if err != nil {
			return err
		}
		if to == model.OrgDeleted {
			// A deleted organization has no members left to sign in with.
			if _, err := tx.Exec(ctx, `DELETE FROM memberships WHERE org_id = $1`, orgID); err != nil {
				return fmt.Errorf("remove members: %w", err)
			}
			if _, err := tx.Exec(ctx, `UPDATE invitations SET status = 'REVOKED' WHERE org_id = $1 AND status = 'PENDING'`, orgID); err != nil {
				return fmt.Errorf("revoke invitations: %w", err)
			}
		}
		return appendAudits(ctx, tx, audit)
	})
	return out, err
}

func (r *TenancyRepository) RetainedData(ctx context.Context, orgID string) ([]string, error) {
	checks := []struct {
		reason, query string
	}{
		{"assets", `SELECT count(*) FROM assets WHERE tenant_id = $1 AND status <> 'DELETED'`},
		{"uploads in progress", `SELECT count(*) FROM upload_sessions WHERE tenant_id = $1 AND status IN ('PENDING', 'UPLOADING', 'COMPLETING', 'ABORTING', 'COMPLETED')`},
		{"unfinished ingestion jobs", `SELECT count(*) FROM ingestion_jobs WHERE tenant_id = $1 AND state NOT IN ('PARTIALLY_COMPLETED', 'COMPLETED', 'FAILED', 'CANCELLED')`},
		{"legal holds", `SELECT count(*) FROM legal_holds WHERE tenant_id = $1 AND released_at IS NULL`},
		{"pending deletions", `SELECT count(*) FROM deletion_operations WHERE tenant_id = $1 AND state NOT IN ('COMPLETED', 'CANCELLED')`},
	}
	var reasons []string
	for _, c := range checks {
		var n int64
		if err := r.db.Pool.QueryRow(ctx, c.query, orgID).Scan(&n); err != nil {
			return nil, fmt.Errorf("check %s: %w", c.reason, err)
		}
		if n > 0 {
			reasons = append(reasons, fmt.Sprintf("%d %s", n, c.reason))
		}
	}
	return reasons, nil
}

// ─── Access ───────────────────────────────────────────────────────────

func (r *TenancyRepository) Access(ctx context.Context, orgID, userID string) (model.Organization, model.Membership, model.Role, error) {
	row := r.db.Pool.QueryRow(ctx, `
		SELECT `+orgColumns+`, `+memberColumns+`
		FROM organizations AS o
		JOIN memberships AS m ON m.org_id = o.id
		JOIN users AS u ON u.id = m.user_id
		WHERE o.id = $1 AND m.user_id = $2
	`, orgID, userID)
	var o model.Organization
	var m model.Membership
	var orgStatus, memberStatus string
	var settings []byte
	err := row.Scan(&o.ID, &o.Name, &o.Slug, &orgStatus, &settings, &o.CreatedBy, &o.CreatedAt, &o.UpdatedAt, &o.DeactivatedAt,
		&m.OrgID, &m.UserID, &m.RoleID, &memberStatus, &m.CreatedBy, &m.CreatedAt, &m.UpdatedAt, &m.Email, &m.DisplayName)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, m, model.Role{}, model.ErrMembershipNotFound
	}
	if err != nil {
		return o, m, model.Role{}, fmt.Errorf("read access: %w", err)
	}
	o.Status, m.Status = model.OrgStatus(orgStatus), model.MembershipStatus(memberStatus)
	if err := json.Unmarshal(settings, &o.Settings); err != nil {
		return o, m, model.Role{}, err
	}
	role, err := r.GetRole(ctx, orgID, m.RoleID)
	if errors.Is(err, model.ErrRoleNotFound) {
		// A role that is gone grants nothing.
		return o, m, model.Role{ID: m.RoleID}, nil
	}
	return o, m, role, err
}

func (r *TenancyRepository) UserMemberships(ctx context.Context, userID string) ([]model.Membership, []model.Organization, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+memberColumns+`, `+orgColumns+`
		FROM memberships AS m
		JOIN users AS u ON u.id = m.user_id
		JOIN organizations AS o ON o.id = m.org_id
		WHERE m.user_id = $1
		ORDER BY o.name, o.id
	`, userID)
	if err != nil {
		return nil, nil, fmt.Errorf("list memberships: %w", err)
	}
	defer rows.Close()
	var members []model.Membership
	var orgs []model.Organization
	for rows.Next() {
		var m model.Membership
		var o model.Organization
		var memberStatus, orgStatus string
		var settings []byte
		if err := rows.Scan(&m.OrgID, &m.UserID, &m.RoleID, &memberStatus, &m.CreatedBy, &m.CreatedAt, &m.UpdatedAt, &m.Email, &m.DisplayName,
			&o.ID, &o.Name, &o.Slug, &orgStatus, &settings, &o.CreatedBy, &o.CreatedAt, &o.UpdatedAt, &o.DeactivatedAt); err != nil {
			return nil, nil, err
		}
		m.Status, o.Status = model.MembershipStatus(memberStatus), model.OrgStatus(orgStatus)
		_ = json.Unmarshal(settings, &o.Settings)
		members = append(members, m)
		orgs = append(orgs, o)
	}
	return members, orgs, rows.Err()
}

// ─── Members ──────────────────────────────────────────────────────────

func (r *TenancyRepository) ListMembers(ctx context.Context, orgID, after string, limit int) ([]model.Membership, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+memberColumns+` FROM memberships AS m JOIN users AS u ON u.id = m.user_id
		WHERE m.org_id = $1 AND m.user_id > $2
		ORDER BY m.user_id LIMIT $3
	`, orgID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}
	return collect(rows, scanMember)
}

func (r *TenancyRepository) ChangeMember(ctx context.Context, c interfaces.MemberChange, audit func(before model.Membership, after *model.Membership) model.AuditEvent) (model.Membership, error) {
	var out model.Membership
	err := withTx(ctx, r.db, func(tx pgx.Tx) error {
		// The organization's lock serializes its membership changes.
		if _, err := scanOrg(tx.QueryRow(ctx, `SELECT `+orgColumns+` FROM organizations AS o WHERE o.id = $1 FOR UPDATE`, c.OrgID)); err != nil {
			return err
		}
		current, err := scanMember(tx.QueryRow(ctx, `
			SELECT `+memberColumns+` FROM memberships AS m JOIN users AS u ON u.id = m.user_id
			WHERE m.org_id = $1 AND m.user_id = $2 FOR UPDATE OF m
		`, c.OrgID, c.UserID))
		if err != nil {
			return err
		}
		if c.Allow != nil {
			if err := c.Allow(current); err != nil {
				return err
			}
		}

		next := current
		if c.RoleID != "" {
			next.RoleID = c.RoleID
		}
		if c.Status != "" {
			next.Status = c.Status
		}
		wasOwner := current.RoleID == model.RoleOwner && current.Status == model.MembershipActive
		staysOwner := !c.Remove && next.RoleID == model.RoleOwner && next.Status == model.MembershipActive
		if wasOwner && !staysOwner {
			// Only owners who can still sign in count.
			var others int
			if err := tx.QueryRow(ctx, `
				SELECT count(*) FROM memberships AS m JOIN users AS u ON u.id = m.user_id
				WHERE m.org_id = $1 AND m.user_id <> $2 AND m.role_id = 'owner' AND m.status = 'ACTIVE' AND u.status = 'ACTIVE'
			`, c.OrgID, c.UserID).Scan(&others); err != nil {
				return fmt.Errorf("count owners: %w", err)
			}
			if others == 0 {
				return model.ErrLastOwner
			}
		}

		if c.Remove {
			if _, err := tx.Exec(ctx, `DELETE FROM memberships WHERE org_id = $1 AND user_id = $2`, c.OrgID, c.UserID); err != nil {
				return fmt.Errorf("remove member: %w", err)
			}
			out = current
			_, err = appendAudit(ctx, tx, audit(current, nil))
			return err
		} else {
			out, err = scanMember(tx.QueryRow(ctx, `
				WITH changed AS (
					UPDATE memberships SET role_id = $3, status = $4, updated_at = $5
					WHERE org_id = $1 AND user_id = $2
					RETURNING *
				)
				SELECT `+memberColumns+` FROM changed AS m JOIN users AS u ON u.id = m.user_id
			`, c.OrgID, c.UserID, next.RoleID, string(next.Status), c.Now))
			if err != nil {
				return fmt.Errorf("change member: %w", err)
			}
		}
		_, err = appendAudit(ctx, tx, audit(current, &out))
		return err
	})
	return out, err
}

// ─── Roles ────────────────────────────────────────────────────────────

func (r *TenancyRepository) CreateRole(ctx context.Context, role model.Role, audit ...model.AuditEvent) error {
	return withTx(ctx, r.db, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO org_roles (id, org_id, name, description, permissions, created_by, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $7)
		`, role.ID, role.OrgID, role.Name, role.Description, role.Permissions.Names(), role.CreatedBy, role.CreatedAt)
		if isConstraint(err, "ux_org_roles_name") {
			return model.ErrRoleExists
		}
		if err != nil {
			return fmt.Errorf("create role: %w", err)
		}
		return appendAudits(ctx, tx, audit)
	})
}

// GetRole returns a built-in role, or the organization's custom role.
func (r *TenancyRepository) GetRole(ctx context.Context, orgID, roleID string) (model.Role, error) {
	if role, ok := model.BuiltInRole(roleID); ok {
		return role, nil
	}
	return scanRole(r.db.Pool.QueryRow(ctx, `
		SELECT `+roleColumns+` FROM org_roles AS r WHERE r.org_id = $1 AND r.id = $2 AND r.deleted_at IS NULL
	`, orgID, roleID))
}

func (r *TenancyRepository) ListRoles(ctx context.Context, orgID string) ([]model.Role, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+roleColumns+` FROM org_roles AS r WHERE r.org_id = $1 AND r.deleted_at IS NULL ORDER BY lower(r.name)
	`, orgID)
	if err != nil {
		return nil, fmt.Errorf("list roles: %w", err)
	}
	return collect(rows, scanRole)
}

func (r *TenancyRepository) UpdateRole(ctx context.Context, role model.Role, audit ...model.AuditEvent) (model.Role, error) {
	var out model.Role
	err := withTx(ctx, r.db, func(tx pgx.Tx) error {
		var err error
		out, err = scanRole(tx.QueryRow(ctx, `
			UPDATE org_roles AS r SET name = $3, description = $4, permissions = $5, updated_at = $6
			WHERE r.org_id = $1 AND r.id = $2 AND r.deleted_at IS NULL
			RETURNING `+roleColumns, role.OrgID, role.ID, role.Name, role.Description, role.Permissions.Names(), role.UpdatedAt))
		if isConstraint(err, "ux_org_roles_name") {
			return model.ErrRoleExists
		}
		if err != nil {
			return err
		}
		return appendAudits(ctx, tx, audit)
	})
	return out, err
}

func (r *TenancyRepository) DeleteRole(ctx context.Context, orgID, roleID string, now time.Time, audit ...model.AuditEvent) error {
	return withTx(ctx, r.db, func(tx pgx.Tx) error {
		if _, err := scanOrg(tx.QueryRow(ctx, `SELECT `+orgColumns+` FROM organizations AS o WHERE o.id = $1 FOR UPDATE`, orgID)); err != nil {
			return err
		}
		var holders int
		if err := tx.QueryRow(ctx, `
			SELECT (SELECT count(*) FROM memberships WHERE org_id = $1 AND role_id = $2)
			     + (SELECT count(*) FROM invitations WHERE org_id = $1 AND role_id = $2 AND status = 'PENDING')
		`, orgID, roleID).Scan(&holders); err != nil {
			return fmt.Errorf("count role holders: %w", err)
		}
		if holders > 0 {
			return model.ErrRoleInUse
		}
		tag, err := tx.Exec(ctx, `
			UPDATE org_roles SET deleted_at = $3, updated_at = $3 WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL
		`, orgID, roleID, now)
		if err != nil {
			return fmt.Errorf("delete role: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return model.ErrRoleNotFound
		}
		return appendAudits(ctx, tx, audit)
	})
}

// ─── Invitations ──────────────────────────────────────────────────────

func (r *TenancyRepository) CreateInvitation(ctx context.Context, inv model.Invitation, tokenHash []byte, audit ...model.AuditEvent) error {
	return withTx(ctx, r.db, func(tx pgx.Tx) error {
		var members int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM memberships AS m JOIN users AS u ON u.id = m.user_id
			WHERE m.org_id = $1 AND lower(u.email) = lower($2)
		`, inv.OrgID, inv.Email).Scan(&members); err != nil {
			return fmt.Errorf("check membership: %w", err)
		}
		if members > 0 {
			return model.ErrMembershipExists
		}
		// Inviting the same address again replaces the earlier invitation.
		if _, err := tx.Exec(ctx, `
			UPDATE invitations SET status = 'REVOKED' WHERE org_id = $1 AND lower(email) = lower($2) AND status = 'PENDING'
		`, inv.OrgID, inv.Email); err != nil {
			return fmt.Errorf("replace invitation: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO invitations (id, org_id, email, role_id, token_hash, status, created_by, created_at, expires_at)
			VALUES ($1, $2, $3, $4, $5, 'PENDING', $6, $7, $8)
		`, inv.ID, inv.OrgID, inv.Email, inv.RoleID, tokenHash, inv.CreatedBy, inv.CreatedAt, inv.ExpiresAt); err != nil {
			return fmt.Errorf("create invitation: %w", err)
		}
		return appendAudits(ctx, tx, audit)
	})
}

func (r *TenancyRepository) ListInvitations(ctx context.Context, orgID, after string, limit int) ([]model.Invitation, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+invitationColumns+` FROM invitations AS i
		WHERE i.org_id = $1 AND ($2 = '' OR (i.created_at, i.id) < (SELECT created_at, id FROM invitations WHERE id = $2))
		ORDER BY i.created_at DESC, i.id DESC LIMIT $3
	`, orgID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("list invitations: %w", err)
	}
	return collect(rows, scanInvitation)
}

func (r *TenancyRepository) RevokeInvitation(ctx context.Context, orgID, invitationID string, now time.Time, audit ...model.AuditEvent) error {
	return withTx(ctx, r.db, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE invitations SET status = 'REVOKED' WHERE org_id = $1 AND id = $2 AND status = 'PENDING'
		`, orgID, invitationID)
		if err != nil {
			return fmt.Errorf("revoke invitation: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return model.ErrInvitationNotFound
		}
		return appendAudits(ctx, tx, audit)
	})
}

func (r *TenancyRepository) AcceptInvitation(ctx context.Context, a interfaces.InvitationAcceptance, audit func(model.Invitation, model.User) []model.AuditEvent) (model.Membership, model.User, error) {
	var member model.Membership
	var user model.User
	err := withTx(ctx, r.db, func(tx pgx.Tx) error {
		inv, err := scanInvitation(tx.QueryRow(ctx, `
			SELECT `+invitationColumns+` FROM invitations AS i
			WHERE i.token_hash = $1 AND i.status = 'PENDING' AND i.expires_at > $2
			FOR UPDATE
		`, a.TokenHash, a.Now))
		if errors.Is(err, model.ErrInvitationNotFound) {
			return model.ErrCredentialToken
		}
		if err != nil {
			return err
		}
		org, err := scanOrg(tx.QueryRow(ctx, `SELECT `+orgColumns+` FROM organizations AS o WHERE o.id = $1 FOR UPDATE`, inv.OrgID))
		if err != nil {
			return err
		}
		if org.Status != model.OrgActive {
			return model.ErrOrgInactive
		}

		if a.NewUser != nil {
			user = *a.NewUser
			_, err := tx.Exec(ctx, `
				INSERT INTO users (id, email, display_name, password_hash, status, password_changed_at, created_at, updated_at)
				VALUES ($1, $2, $3, $4, 'ACTIVE', $5, $5, $5)
			`, user.ID, inv.Email, user.DisplayName, user.PasswordHash, a.Now)
			if isConstraint(err, "ux_users_email") {
				return model.ErrEmailTaken
			}
			if err != nil {
				return fmt.Errorf("create user: %w", err)
			}
			user.Email, user.Status = inv.Email, model.UserActive
		} else {
			user, err = scanUser(tx.QueryRow(ctx, `SELECT `+userColumns+` FROM users AS u WHERE u.id = $1 FOR UPDATE`, a.UserID))
			if err != nil {
				return err
			}
			// The invitation is for an address, not for whoever holds the
			// token: the signed-in account must own that address.
			if user.Status != model.UserActive || !strings.EqualFold(user.Email, inv.Email) {
				return model.ErrForbidden
			}
		}

		member = model.Membership{
			OrgID: inv.OrgID, UserID: user.ID, RoleID: inv.RoleID, Status: model.MembershipActive,
			CreatedBy: inv.CreatedBy, CreatedAt: a.Now, UpdatedAt: a.Now, Email: user.Email, DisplayName: user.DisplayName,
		}
		if err := insertMember(ctx, tx, member); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE invitations SET status = 'ACCEPTED', accepted_at = $2, accepted_by = $3 WHERE id = $1
		`, inv.ID, a.Now, user.ID); err != nil {
			return fmt.Errorf("accept invitation: %w", err)
		}
		return appendAudits(ctx, tx, audit(inv, user))
	})
	return member, user, err
}

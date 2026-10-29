package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// SSORepository stores organizations' identity providers, sign-ins in
// progress and the links from providers' subjects to accounts.
type SSORepository struct {
	db *DB
}

func NewSSORepository(db *DB) *SSORepository {
	return &SSORepository{db: db}
}

func (r *SSORepository) GetSSO(ctx context.Context, orgID string) (model.SSOConnection, error) {
	var c model.SSOConnection
	err := r.db.Pool.QueryRow(ctx, `
		SELECT org_id, protocol, issuer, client_id, COALESCE(idp_metadata, ''), COALESCE(secret_key_version, 0),
		       secret_nonce, secret_ciphertext, allowed_domains, default_role, enabled, updated_by, created_at, updated_at
		FROM sso_connections WHERE org_id = $1`, orgID).Scan(
		&c.OrgID, &c.Protocol, &c.Issuer, &c.ClientID, &c.IDPMetadata, &c.SecretKeyVersion,
		&c.SecretNonce, &c.SecretCipher, &c.AllowedDomains, &c.DefaultRole, &c.Enabled, &c.UpdatedBy, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, model.ErrSSONotConfigured
	}
	return c, err
}

func (r *SSORepository) PutSSO(ctx context.Context, c model.SSOConnection, audit ...model.AuditEvent) error {
	return withTx(ctx, r.db, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO sso_connections (org_id, issuer, client_id, secret_key_version, secret_nonce, secret_ciphertext,
			                             allowed_domains, default_role, enabled, updated_by, created_at, updated_at,
			                             protocol, idp_metadata)
			VALUES ($1, $2, $3, NULLIF($4, 0), $5, $6, $7, $8, $9, $10, $11, $11, $12, NULLIF($13, ''))
			ON CONFLICT (org_id) DO UPDATE SET issuer = $2, client_id = $3, secret_key_version = NULLIF($4, 0),
			    secret_nonce = $5, secret_ciphertext = $6, allowed_domains = $7, default_role = $8, enabled = $9,
			    updated_by = $10, updated_at = $11, protocol = $12, idp_metadata = NULLIF($13, '')`,
			c.OrgID, c.Issuer, c.ClientID, c.SecretKeyVersion, c.SecretNonce, c.SecretCipher,
			c.AllowedDomains, c.DefaultRole, c.Enabled, c.UpdatedBy, c.UpdatedAt, c.Protocol, c.IDPMetadata)
		if err != nil {
			return fmt.Errorf("save sso connection: %w", err)
		}
		return appendAudits(ctx, tx, audit)
	})
}

func (r *SSORepository) DeleteSSO(ctx context.Context, orgID string, audit ...model.AuditEvent) error {
	return withTx(ctx, r.db, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM sso_connections WHERE org_id = $1`, orgID)
		if err != nil {
			return fmt.Errorf("delete sso connection: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return model.ErrSSONotConfigured
		}
		return appendAudits(ctx, tx, audit)
	})
}

func (r *SSORepository) OrganizationBySlug(ctx context.Context, slug string) (model.Organization, error) {
	return scanOrg(r.db.Pool.QueryRow(ctx, `SELECT `+orgColumns+` FROM organizations AS o WHERE o.slug = $1`, slug))
}

// SaveLoginState stores a started sign-in, and clears ones that expired.
func (r *SSORepository) SaveLoginState(ctx context.Context, stateHash []byte, st model.SSOLoginState, now time.Time) error {
	if _, err := r.db.Pool.Exec(ctx, `DELETE FROM sso_login_states WHERE expires_at < $1`, now); err != nil {
		return fmt.Errorf("clear expired sso states: %w", err)
	}
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO sso_login_states (state_hash, org_id, nonce, verifier, redirect_uri, expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, stateHash, st.OrgID, st.Nonce, st.Verifier, st.RedirectURI, st.ExpiresAt, now)
	if err != nil {
		return fmt.Errorf("save sso state: %w", err)
	}
	return nil
}

// TakeLoginState spends a sign-in's state: it works once, before it
// expires.
func (r *SSORepository) TakeLoginState(ctx context.Context, stateHash []byte, now time.Time) (model.SSOLoginState, error) {
	var st model.SSOLoginState
	err := r.db.Pool.QueryRow(ctx, `
		DELETE FROM sso_login_states WHERE state_hash = $1
		RETURNING org_id, nonce, verifier, redirect_uri, expires_at`, stateHash).Scan(
		&st.OrgID, &st.Nonce, &st.Verifier, &st.RedirectURI, &st.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !st.ExpiresAt.After(now)) {
		return st, fmt.Errorf("%w: the sign-in expired or was already used; start again", model.ErrSSOFailed)
	}
	return st, err
}

// LinkedUser is the account a provider's subject signed in as before, or
// "".
func (r *SSORepository) LinkedUser(ctx context.Context, issuer, subject string) (string, error) {
	var userID string
	err := r.db.Pool.QueryRow(ctx, `SELECT user_id FROM sso_identities WHERE issuer = $1 AND subject = $2`, issuer, subject).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return userID, err
}

// Provision writes a first sign-in's account, link and membership together.
func (r *SSORepository) Provision(ctx context.Context, p model.SSOProvision, now time.Time, audit ...model.AuditEvent) error {
	return withTx(ctx, r.db, func(tx pgx.Tx) error {
		if p.NewUser {
			u := p.User
			_, err := tx.Exec(ctx, `
				INSERT INTO users (id, email, display_name, status, sso_only, created_at, updated_at)
				VALUES ($1, $2, $3, $4, true, $5, $5)`, u.ID, u.Email, u.DisplayName, string(u.Status), u.CreatedAt)
			if isConstraint(err, "ux_users_email") {
				return model.ErrEmailTaken
			}
			if err != nil {
				return fmt.Errorf("create sso user: %w", err)
			}
		}
		if p.Issuer == "" {
			// SCIM provisioning: an account and a membership, no sign-in yet
			// to link.
			if p.Member != nil {
				if err := insertMember(ctx, tx, *p.Member); err != nil {
					return err
				}
			}
			return appendAudits(ctx, tx, audit)
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO sso_identities (issuer, subject, user_id, created_at) VALUES ($1, $2, $3, $4)
			ON CONFLICT (issuer, subject) DO NOTHING`, p.Issuer, p.Subject, p.User.ID, now)
		if isConstraint(err, "sso_identities_issuer_user_id_key") {
			return fmt.Errorf("%w: this account is already linked to another identity at the provider", model.ErrSSOFailed)
		}
		if err != nil {
			return fmt.Errorf("link sso identity: %w", err)
		}
		var linked string
		if err := tx.QueryRow(ctx, `SELECT user_id FROM sso_identities WHERE issuer = $1 AND subject = $2`, p.Issuer, p.Subject).Scan(&linked); err != nil {
			return fmt.Errorf("read sso identity: %w", err)
		}
		if linked != p.User.ID {
			return fmt.Errorf("%w: this identity is linked to another account", model.ErrSSOFailed)
		}
		if p.Member != nil {
			if err := insertMember(ctx, tx, *p.Member); err != nil && !errors.Is(err, model.ErrMembershipExists) {
				return err
			}
		}
		return appendAudits(ctx, tx, audit)
	})
}

// ─── SCIM tokens ──────────────────────────────────────────────────────

// PutSCIMToken stores an organization's SCIM token hash, replacing any.
func (r *SSORepository) PutSCIMToken(ctx context.Context, orgID string, hash []byte, actor string, now time.Time, audit ...model.AuditEvent) error {
	return withTx(ctx, r.db, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO scim_tokens (org_id, token_hash, created_by, created_at) VALUES ($1, $2, $3, $4)
			ON CONFLICT (org_id) DO UPDATE SET token_hash = $2, created_by = $3, created_at = $4, last_used_at = NULL`,
			orgID, hash, actor, now)
		if err != nil {
			return fmt.Errorf("save scim token: %w", err)
		}
		return appendAudits(ctx, tx, audit)
	})
}

func (r *SSORepository) DeleteSCIMToken(ctx context.Context, orgID string, audit ...model.AuditEvent) error {
	return withTx(ctx, r.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM scim_tokens WHERE org_id = $1`, orgID); err != nil {
			return fmt.Errorf("delete scim token: %w", err)
		}
		return appendAudits(ctx, tx, audit)
	})
}

// SCIMToken says when the organization's token was made and last used;
// found is false when it has none.
func (r *SSORepository) SCIMToken(ctx context.Context, orgID string) (created time.Time, lastUsed *time.Time, found bool, err error) {
	err = r.db.Pool.QueryRow(ctx, `SELECT created_at, last_used_at FROM scim_tokens WHERE org_id = $1`, orgID).Scan(&created, &lastUsed)
	if errors.Is(err, pgx.ErrNoRows) {
		return created, nil, false, nil
	}
	return created, lastUsed, err == nil, err
}

// OrgBySCIMToken is the organization a token belongs to, or "".
func (r *SSORepository) OrgBySCIMToken(ctx context.Context, hash []byte, now time.Time) (string, error) {
	var orgID string
	err := r.db.Pool.QueryRow(ctx, `
		UPDATE scim_tokens SET last_used_at = $2 WHERE token_hash = $1 RETURNING org_id`, hash, now).Scan(&orgID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return orgID, err
}

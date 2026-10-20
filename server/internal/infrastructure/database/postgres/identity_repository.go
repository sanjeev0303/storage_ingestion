package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// IdentityRepository stores accounts, sessions and credential tokens.
// Tokens arrive hashed; password hashes stay inside this package and the
// identity service.
type IdentityRepository struct {
	db *DB
}

func NewIdentityRepository(db *DB) *IdentityRepository {
	return &IdentityRepository{db: db}
}

const userColumns = `u.id, u.email, u.display_name, COALESCE(u.password_hash, ''), u.status, COALESCE(u.platform_role, ''),
	u.password_changed_at, u.last_login_at, u.created_at, u.updated_at`

func scanUser(row pgx.Row) (model.User, error) {
	var u model.User
	var status, role string
	err := row.Scan(&u.ID, &u.Email, &u.DisplayName, &u.PasswordHash, &status, &role,
		&u.PasswordChangedAt, &u.LastLoginAt, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return u, model.ErrUserNotFound
	}
	u.Status, u.PlatformRole = model.UserStatus(status), model.PlatformRole(role)
	return u, err
}

const sessionColumns = `s.id, s.user_id, s.access_expires_at, s.expires_at, COALESCE(s.source_ip, ''),
	COALESCE(s.user_agent, ''), s.created_at, s.last_used_at, s.revoked_at, COALESCE(s.revoke_reason, '')`

func scanSession(row pgx.Row) (model.Session, error) {
	var s model.Session
	err := row.Scan(&s.ID, &s.UserID, &s.AccessExpiresAt, &s.ExpiresAt, &s.SourceIP, &s.UserAgent,
		&s.CreatedAt, &s.LastUsedAt, &s.RevokedAt, &s.RevokeReason)
	return s, err
}

func isConstraint(err error, name string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.ConstraintName == name
}

func (r *IdentityRepository) CreateUser(ctx context.Context, u model.User, audit ...model.AuditEvent) error {
	return withTx(ctx, r.db, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO users (id, email, display_name, password_hash, status, platform_role, password_changed_at,
			                   created_at, updated_at)
			VALUES ($1, $2, $3, NULLIF($4, ''), $5, NULLIF($6, ''), $7, $8, $8)
		`, u.ID, u.Email, u.DisplayName, u.PasswordHash, string(u.Status), string(u.PlatformRole), u.PasswordChangedAt, u.CreatedAt)
		if isConstraint(err, "ux_users_email") {
			return model.ErrEmailTaken
		}
		if err != nil {
			return fmt.Errorf("create user: %w", err)
		}
		return appendAudits(ctx, tx, audit)
	})
}

func (r *IdentityRepository) GetUser(ctx context.Context, userID string) (model.User, error) {
	return scanUser(r.db.Pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users AS u WHERE u.id = $1`, userID))
}

func (r *IdentityRepository) GetUserByEmail(ctx context.Context, email string) (model.User, error) {
	return scanUser(r.db.Pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users AS u WHERE lower(u.email) = lower($1)`, email))
}

func (r *IdentityRepository) ListUsers(ctx context.Context, after string, limit int) ([]model.User, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+userColumns+` FROM users AS u WHERE u.id > $1 ORDER BY u.id LIMIT $2
	`, after, limit)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	return collect(rows, scanUser)
}

func (r *IdentityRepository) CountPlatformAdmins(ctx context.Context) (int, error) {
	var n int
	err := r.db.Pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE platform_role = 'ADMIN' AND status = 'ACTIVE'`).Scan(&n)
	return n, err
}

func (r *IdentityRepository) SetUserStatus(ctx context.Context, userID string, status model.UserStatus, now time.Time, audit ...model.AuditEvent) (model.User, error) {
	var u model.User
	err := withTx(ctx, r.db, func(tx pgx.Tx) error {
		var err error
		u, err = scanUser(tx.QueryRow(ctx, `
			UPDATE users AS u SET status = $2, updated_at = $3 WHERE u.id = $1 RETURNING `+userColumns,
			userID, string(status), now))
		if err != nil {
			return err
		}
		if status == model.UserDeactivated {
			if _, err := tx.Exec(ctx, `
				UPDATE auth_sessions SET revoked_at = $2, revoke_reason = 'account deactivated'
				WHERE user_id = $1 AND revoked_at IS NULL
			`, userID, now); err != nil {
				return fmt.Errorf("revoke sessions: %w", err)
			}
		}
		return appendAudits(ctx, tx, audit)
	})
	return u, err
}

func (r *IdentityRepository) SetPlatformRole(ctx context.Context, userID string, role model.PlatformRole, now time.Time, audit ...model.AuditEvent) error {
	return withTx(ctx, r.db, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE users SET platform_role = NULLIF($2, ''), updated_at = $3 WHERE id = $1`,
			userID, string(role), now)
		if err != nil {
			return fmt.Errorf("set platform role: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return model.ErrUserNotFound
		}
		return appendAudits(ctx, tx, audit)
	})
}

func (r *IdentityRepository) ChangePassword(ctx context.Context, userID, passwordHash, keepSessionID string, now time.Time, audit ...model.AuditEvent) error {
	return withTx(ctx, r.db, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE users SET password_hash = $2, password_changed_at = $3, updated_at = $3 WHERE id = $1
		`, userID, passwordHash, now)
		if err != nil {
			return fmt.Errorf("change password: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return model.ErrUserNotFound
		}
		if _, err := tx.Exec(ctx, `
			UPDATE auth_sessions SET revoked_at = $3, revoke_reason = 'password changed'
			WHERE user_id = $1 AND id <> $2 AND revoked_at IS NULL
		`, userID, keepSessionID, now); err != nil {
			return fmt.Errorf("revoke other sessions: %w", err)
		}
		return appendAudits(ctx, tx, audit)
	})
}

func (r *IdentityRepository) RehashPassword(ctx context.Context, userID, passwordHash string) error {
	_, err := r.db.Pool.Exec(ctx, `UPDATE users SET password_hash = $2 WHERE id = $1 AND status = 'ACTIVE'`, userID, passwordHash)
	return err
}

func (r *IdentityRepository) RecordLogin(ctx context.Context, userID string, now time.Time) error {
	_, err := r.db.Pool.Exec(ctx, `UPDATE users SET last_login_at = $2 WHERE id = $1`, userID, now)
	return err
}

func (r *IdentityRepository) CreateSession(ctx context.Context, s model.Session, accessHash, refreshHash []byte, audit ...model.AuditEvent) error {
	return withTx(ctx, r.db, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO auth_sessions (id, user_id, access_hash, access_expires_at, expires_at, source_ip, user_agent,
			                           created_at, last_used_at)
			VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), NULLIF($7, ''), $8, $8)
		`, s.ID, s.UserID, accessHash, s.AccessExpiresAt, s.ExpiresAt, s.SourceIP, clipText(s.UserAgent), s.CreatedAt)
		if err != nil {
			return fmt.Errorf("create session: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO auth_refresh_tokens (token_hash, session_id, created_at) VALUES ($1, $2, $3)
		`, refreshHash, s.ID, s.CreatedAt); err != nil {
			return fmt.Errorf("store refresh token: %w", err)
		}
		return appendAudits(ctx, tx, audit)
	})
}

func (r *IdentityRepository) SessionByAccess(ctx context.Context, accessHash []byte, now time.Time) (model.Session, model.User, error) {
	row := r.db.Pool.QueryRow(ctx, `
		SELECT `+sessionColumns+`, `+userColumns+`
		FROM auth_sessions AS s JOIN users AS u ON u.id = s.user_id
		WHERE s.access_hash = $1 AND s.revoked_at IS NULL AND s.access_expires_at > $2 AND s.expires_at > $2
	`, accessHash, now)
	var s model.Session
	var u model.User
	var status, role string
	err := row.Scan(&s.ID, &s.UserID, &s.AccessExpiresAt, &s.ExpiresAt, &s.SourceIP, &s.UserAgent,
		&s.CreatedAt, &s.LastUsedAt, &s.RevokedAt, &s.RevokeReason,
		&u.ID, &u.Email, &u.DisplayName, &u.PasswordHash, &status, &role,
		&u.PasswordChangedAt, &u.LastLoginAt, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, u, model.ErrSessionInvalid
	}
	if err != nil {
		return s, u, fmt.Errorf("read session: %w", err)
	}
	u.Status, u.PlatformRole = model.UserStatus(status), model.PlatformRole(role)
	// At most one write a minute per session, so reads stay reads.
	_, _ = r.db.Pool.Exec(ctx, `
		UPDATE auth_sessions SET last_used_at = $2 WHERE id = $1 AND last_used_at < $2 - interval '1 minute'
	`, s.ID, now)
	return s, u, nil
}

func (r *IdentityRepository) RotateRefresh(ctx context.Context, refreshHash, newAccessHash, newRefreshHash []byte, accessExpiresAt, now time.Time) (model.Session, model.User, error) {
	var s model.Session
	var u model.User
	replayed := false
	err := withTx(ctx, r.db, func(tx pgx.Tx) error {
		var sessionID string
		var usedAt *time.Time
		err := tx.QueryRow(ctx, `
			SELECT session_id, used_at FROM auth_refresh_tokens WHERE token_hash = $1 FOR UPDATE
		`, refreshHash).Scan(&sessionID, &usedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return model.ErrSessionInvalid
		}
		if err != nil {
			return fmt.Errorf("read refresh token: %w", err)
		}
		s, err = scanSession(tx.QueryRow(ctx, `SELECT `+sessionColumns+` FROM auth_sessions AS s WHERE s.id = $1 FOR UPDATE`, sessionID))
		if err != nil {
			return fmt.Errorf("lock session: %w", err)
		}
		if s.RevokedAt != nil || !s.ExpiresAt.After(now) {
			return model.ErrSessionInvalid
		}
		if usedAt != nil {
			// A spent token came back: someone holds a copy. End the
			// session for both holders; the revocation is committed.
			replayed = true
			_, err := tx.Exec(ctx, `
				UPDATE auth_sessions SET revoked_at = $2, revoke_reason = 'refresh token reuse' WHERE id = $1
			`, s.ID, now)
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE auth_refresh_tokens SET used_at = $2 WHERE token_hash = $1`, refreshHash, now); err != nil {
			return fmt.Errorf("spend refresh token: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO auth_refresh_tokens (token_hash, session_id, created_at) VALUES ($1, $2, $3)
		`, newRefreshHash, s.ID, now); err != nil {
			return fmt.Errorf("store refresh token: %w", err)
		}
		s, err = scanSession(tx.QueryRow(ctx, `
			UPDATE auth_sessions AS s
			SET access_hash = $2, access_expires_at = LEAST($3, s.expires_at), last_used_at = $4
			WHERE s.id = $1
			RETURNING `+sessionColumns, s.ID, newAccessHash, accessExpiresAt, now))
		if err != nil {
			return fmt.Errorf("rotate access token: %w", err)
		}
		u, err = scanUser(tx.QueryRow(ctx, `SELECT `+userColumns+` FROM users AS u WHERE u.id = $1`, s.UserID))
		return err
	})
	if err == nil && replayed {
		return s, u, model.ErrRefreshReplayed
	}
	return s, u, err
}

func (r *IdentityRepository) RevokeSession(ctx context.Context, userID, sessionID, reason string, now time.Time, audit ...model.AuditEvent) error {
	return withTx(ctx, r.db, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE auth_sessions SET revoked_at = $3, revoke_reason = $4
			WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL
		`, sessionID, userID, now, reason)
		if err != nil {
			return fmt.Errorf("revoke session: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return model.ErrSessionInvalid
		}
		return appendAudits(ctx, tx, audit)
	})
}

func (r *IdentityRepository) ListSessions(ctx context.Context, userID string, now time.Time) ([]model.Session, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+sessionColumns+` FROM auth_sessions AS s
		WHERE s.user_id = $1 AND s.revoked_at IS NULL AND s.expires_at > $2
		ORDER BY s.created_at DESC LIMIT 100
	`, userID, now)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	return collect(rows, scanSession)
}

func (r *IdentityRepository) CreateCredentialToken(ctx context.Context, tokenHash []byte, userID, purpose, createdBy string, expiresAt, now time.Time, audit ...model.AuditEvent) error {
	return withTx(ctx, r.db, func(tx pgx.Tx) error {
		// Only the newest token of a purpose works.
		if _, err := tx.Exec(ctx, `
			UPDATE auth_credential_tokens SET used_at = $3 WHERE user_id = $1 AND purpose = $2 AND used_at IS NULL
		`, userID, purpose, now); err != nil {
			return fmt.Errorf("retire credential tokens: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO auth_credential_tokens (token_hash, user_id, purpose, created_by, created_at, expires_at)
			VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6)
		`, tokenHash, userID, purpose, createdBy, now, expiresAt); err != nil {
			return fmt.Errorf("store credential token: %w", err)
		}
		return appendAudits(ctx, tx, audit)
	})
}

func (r *IdentityRepository) ConsumeCredentialToken(ctx context.Context, tokenHash []byte, purpose, passwordHash string, now time.Time, audit func(model.User) model.AuditEvent) (model.User, error) {
	var u model.User
	err := withTx(ctx, r.db, func(tx pgx.Tx) error {
		var userID string
		err := tx.QueryRow(ctx, `
			SELECT user_id FROM auth_credential_tokens
			WHERE token_hash = $1 AND purpose = $2 AND used_at IS NULL AND expires_at > $3
			FOR UPDATE
		`, tokenHash, purpose, now).Scan(&userID)
		if errors.Is(err, pgx.ErrNoRows) {
			return model.ErrCredentialToken
		}
		if err != nil {
			return fmt.Errorf("read credential token: %w", err)
		}
		u, err = scanUser(tx.QueryRow(ctx, `SELECT `+userColumns+` FROM users AS u WHERE u.id = $1 FOR UPDATE`, userID))
		if err != nil {
			return err
		}
		switch {
		case u.Status == model.UserDeactivated:
			return model.ErrAccountInactive
		case purpose == model.CredentialActivate && u.Status != model.UserPending:
			return model.ErrCredentialToken
		case purpose == model.CredentialReset && u.Status != model.UserActive:
			return model.ErrCredentialToken
		}
		u, err = scanUser(tx.QueryRow(ctx, `
			UPDATE users AS u SET password_hash = $2, password_changed_at = $3, status = 'ACTIVE', updated_at = $3
			WHERE u.id = $1 RETURNING `+userColumns, userID, passwordHash, now))
		if err != nil {
			return fmt.Errorf("set password: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE auth_credential_tokens SET used_at = $2 WHERE token_hash = $1`, tokenHash, now); err != nil {
			return fmt.Errorf("spend credential token: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE auth_sessions SET revoked_at = $2, revoke_reason = 'password reset' WHERE user_id = $1 AND revoked_at IS NULL
		`, userID, now); err != nil {
			return fmt.Errorf("revoke sessions: %w", err)
		}
		_, err = appendAudit(ctx, tx, audit(u))
		return err
	})
	return u, err
}

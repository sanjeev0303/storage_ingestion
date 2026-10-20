// Command identity-admin is the operator's tool for accounts,
// organizations, the audit log and quotas. It works with direct database
// access, so whoever runs it is a platform operator; everything it changes
// is audited like the API's changes.
//
// bootstrap creates the first platform administrator. With no
// -password-file it generates a random password and prints it once:
// there is no default password anywhere.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/config"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/ratelimit"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
	"github.com/sanjeev0303/object-storage-platform/internal/service/identity"
	"github.com/sanjeev0303/object-storage-platform/internal/service/quota"
)

const usage = `usage: identity-admin <command> [flags]

commands:
  bootstrap        create the first platform administrator (-email [-name] [-password-file])
  reset-password   issue a single-use password-reset token for an account (-email)
  deactivate       deactivate an account and revoke its sessions (-email)
  reactivate       reactivate an account (-email)
  create-org       create an organization with an owner (-name -slug -owner-email)
  audit-verify     check the audit log's hash chains (-chain, default all)
  audit-purge      remove audit events older than AUDIT_RETENTION (-limit)
  quota-reconcile  settle stale reservations and recount usage (-tenant, default all)

Run "identity-admin <command> -h" for a command's flags.`

func main() {
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "help" {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "identity-admin:", err)
		os.Exit(1)
	}
}

func run(command string, args []string, out io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	sec, err := config.LoadSecurity()
	if err != nil {
		return fmt.Errorf("load security configuration: %w", err)
	}
	db, err := postgres.Open(ctx, cfg.DatabaseUrl)
	if err != nil {
		return fmt.Errorf("connect to PostgreSQL: %w", err)
	}
	defer db.Close()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	auditService := audit.NewService(postgres.NewAuditRepository(db), log, nil, sec.Audit.Retention)
	identityService, err := identity.NewService(identity.Dependencies{
		Users: postgres.NewIdentityRepository(db), Tenancy: postgres.NewTenancyRepository(db),
		Audit: auditService, Limiter: ratelimit.New(), Log: log,
	}, sec)
	if err != nil {
		return err
	}
	operator := model.Principal{UserID: "operator-cli", Service: true, PlatformRole: model.PlatformAdmin}

	flags := flag.NewFlagSet(command, flag.ExitOnError)
	email := flags.String("email", "", "account email")
	name := flags.String("name", "", "display name, or the organization's name")
	passwordFile := flags.String("password-file", "", "read the bootstrap password from this file instead of generating one")
	slug := flags.String("slug", "", "organization slug")
	ownerEmail := flags.String("owner-email", "", "email of the organization's first owner")
	chain := flags.String("chain", "", "audit chain: a tenant ID, or _platform")
	limit := flags.Int("limit", 10000, "most audit events to purge")
	tenant := flags.String("tenant", "", "tenant to reconcile")
	if err := flags.Parse(args); err != nil {
		return err
	}

	switch command {
	case "bootstrap":
		pw := ""
		if *passwordFile != "" {
			raw, err := os.ReadFile(*passwordFile)
			if err != nil {
				return err
			}
			pw = strings.TrimRight(string(raw), "\r\n")
		}
		user, generated, err := identityService.Bootstrap(ctx, *email, *name, pw)
		if errors.Is(err, identity.ErrBootstrapDone) {
			return fmt.Errorf("%w; use the API or reset-password", err)
		}
		if err != nil {
			return err
		}
		result := map[string]any{"user_id": user.ID, "email": user.Email, "platform_role": user.PlatformRole}
		if pw == "" {
			// Printed once: it is stored only as an Argon2id hash.
			result["password"] = generated
			result["note"] = "store this password now; it is not shown again"
		}
		return printJSON(out, result)

	case "reset-password":
		token, err := identityService.IssueResetByEmail(ctx, *email)
		if err != nil {
			return err
		}
		return printJSON(out, map[string]any{
			"user_id": token.UserID, "token": token.Token, "expires_at": token.ExpiresAt,
			"use": "POST /api/v1/auth/password/reset (or /api/v1/auth/activate for a pending account)",
		})

	case "deactivate", "reactivate":
		user, err := identityService.SetUserStatusByEmail(ctx, *email, command == "reactivate")
		if err != nil {
			return err
		}
		return printJSON(out, map[string]any{"user_id": user.ID, "status": user.Status})

	case "create-org":
		owner, err := postgres.NewIdentityRepository(db).GetUserByEmail(ctx, *ownerEmail)
		if err != nil {
			return fmt.Errorf("owner: %w", err)
		}
		org, err := identityService.CreateOrganization(ctx, operator, *name, *slug, owner.ID)
		if err != nil {
			return err
		}
		return printJSON(out, map[string]any{"organization_id": org.ID, "slug": org.Slug, "owner": owner.ID})

	case "audit-verify":
		var results []any
		if *chain != "" {
			v, err := postgres.NewAuditRepository(db).Verify(ctx, *chain)
			if err != nil {
				return err
			}
			results = append(results, v)
		} else {
			all, err := auditService.VerifyAll(ctx)
			if err != nil {
				return err
			}
			for _, v := range all {
				results = append(results, v)
			}
		}
		return printJSON(out, results)

	case "audit-purge":
		n, err := auditService.Purge(ctx, *limit)
		if err != nil {
			return err
		}
		return printJSON(out, map[string]any{"purged": n, "retention": sec.Audit.Retention.String()})

	case "quota-reconcile":
		quotas := quota.NewService(postgres.NewQuotaRepository(db), sec.Quotas, auditService, log)
		if *tenant != "" {
			r, err := quotas.Reconcile(ctx, *tenant)
			if err != nil {
				return err
			}
			return printJSON(out, r)
		}
		results, err := quotas.ReconcileAll(ctx)
		if err != nil {
			return err
		}
		return printJSON(out, results)
	}
	return fmt.Errorf("unknown command %q\n%s", command, usage)
}

func printJSON(out io.Writer, value any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}

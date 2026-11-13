// Command credential-admin is the operator's tool for the sealed storage
// credentials of customer-managed connections (docs/storage-connections.md).
//
// reseal moves every stored credential into an envelope under the newest
// master key in STORAGE_CREDENTIAL_KEYS: each credential's data key is
// wrapped again, and a credential sealed before envelopes is sealed in one.
// It works in small transactions, can be stopped and run again at any
// time, and never prints a credential.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/config"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/secretbox"
)

const usage = `usage: credential-admin <command> [flags]

commands:
  status   count stored credentials by master key version
  reseal   re-wrap every credential under the newest key (-dry-run, -batch, -after)

Run "credential-admin <command> -h" for a command's flags.`

func main() {
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "help" {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "credential-admin:", err)
		os.Exit(1)
	}
}

func run(command string, args []string, out io.Writer) error {
	flags := flag.NewFlagSet(command, flag.ExitOnError)
	dryRun := flags.Bool("dry-run", false, "check every credential opens and count what would change, without writing")
	batch := flags.Int("batch", 100, "connections per transaction")
	after := flags.String("after", "", "start after this connection ID (to resume a stopped run)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if command != "status" && command != "reseal" {
		return fmt.Errorf("unknown command %q\n\n%s", command, usage)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	db, err := postgres.Open(ctx, cfg.DatabaseUrl)
	if err != nil {
		return fmt.Errorf("connect to PostgreSQL: %w", err)
	}
	defer db.Close()
	repo := postgres.NewStorageConnectionRepository(db)

	if command == "status" {
		use, err := repo.CredentialKeyUse(ctx)
		if err != nil {
			return err
		}
		return printJSON(out, map[string]any{"credentials": use})
	}

	storage, err := config.LoadStorage()
	if err != nil {
		return fmt.Errorf("load storage configuration: %w", err)
	}
	if !storage.BYOSEnabled {
		return errors.New("BYOS_ENABLED is off: there are no credential keys to reseal with")
	}
	keyring, err := storage.Keyring()
	if err != nil {
		return err
	}
	report, err := reseal(ctx, repo, secretbox.NewEnvelope(keyring), keyring.CurrentVersion(), *after, *batch, !*dryRun, os.Stderr)
	if err != nil {
		return err
	}
	return printJSON(out, report)
}

// store is what reseal needs from the connection repository.
type store interface {
	ResealBatch(ctx context.Context, keyVersion int, after string, limit int, write bool,
		reseal func(*postgres.SealedCredentials) error) (string, int, error)
	EraseDeletedCredentials(ctx context.Context) (int64, error)
}

type resealReport struct {
	KeyVersion int      `json:"key_version"`
	DryRun     bool     `json:"dry_run"`
	Resealed   int      `json:"resealed"`
	Failed     []string `json:"failed,omitempty"` // connection IDs whose credentials did not open
	Erased     int64    `json:"erased_from_deleted"`
}

// reseal walks the connections in ID order, a batch per transaction,
// until none is left after the last one seen. A connection whose
// credentials do not open (a key version that is no longer configured) is
// left as it was and listed; the walk goes on past it.
func reseal(ctx context.Context, repo store, envelope *secretbox.Envelope, keyVersion int, after string, batch int, write bool, progress io.Writer) (resealReport, error) {
	report := resealReport{KeyVersion: keyVersion, DryRun: !write}
	if batch <= 0 {
		return report, errors.New("-batch must be positive")
	}
	if write {
		erased, err := repo.EraseDeletedCredentials(ctx)
		if err != nil {
			return report, err
		}
		report.Erased = erased
	}
	for {
		var failedHere []string
		last, resealed, err := repo.ResealBatch(ctx, keyVersion, after, batch, write, func(s *postgres.SealedCredentials) error {
			err := rewrapAll(envelope, s)
			if err != nil {
				failedHere = append(failedHere, s.Connection.ID)
			}
			return err
		})
		if err != nil {
			return report, fmt.Errorf("after %q: %w", after, err)
		}
		if last == "" {
			return report, nil
		}
		report.Resealed += resealed
		report.Failed = append(report.Failed, failedHere...)
		fmt.Fprintf(progress, "resealed %d, failed %d, up to %s\n", report.Resealed, len(report.Failed), last)
		after = last
	}
}

// rewrapAll re-wraps a connection's current, staged and previous
// credentials, each under its own credential version's associated data.
func rewrapAll(envelope *secretbox.Envelope, s *postgres.SealedCredentials) error {
	for _, cred := range []*interfaces.SealedCredential{&s.Current, &s.Staged, &s.Previous} {
		if cred.Version == 0 || len(cred.Ciphertext) == 0 {
			continue
		}
		sealed := secretbox.Sealed{KeyVersion: cred.KeyVersion, Nonce: cred.Nonce, Ciphertext: cred.Ciphertext, WrappedKey: cred.WrappedKey}
		moved, err := envelope.Rewrap(sealed, s.Connection.CredentialAAD(cred.Version))
		if err != nil {
			return err
		}
		cred.KeyVersion, cred.Nonce, cred.Ciphertext, cred.WrappedKey = moved.KeyVersion, moved.Nonce, moved.Ciphertext, moved.WrappedKey
	}
	return nil
}

func printJSON(out io.Writer, value any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}

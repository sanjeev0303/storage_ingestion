// Package migrate applies the schema migrations with the same guarantees
// as scripts/migrate.ps1, from any machine that reaches the database: one
// run at a time (an advisory lock), and each file applied and recorded in
// one transaction, or neither.
package migrate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// lockKey is the advisory lock scripts/migrate.ps1 takes too, so the two
// never run at once.
const lockKey = 7240512

type Status struct {
	Name    string
	Applied bool
	HasDown bool
}

// Migrator runs migrations from up (and rollbacks from down) on one
// connection.
type Migrator struct {
	Conn *pgx.Conn
	Up   fs.FS
	Down fs.FS
	// Log receives one line per file applied; nil is silent.
	Log func(string)
}

func (m *Migrator) say(format string, args ...any) {
	if m.Log != nil {
		m.Log(fmt.Sprintf(format, args...))
	}
}

func files(fsys fs.FS, dir string) ([]string, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// locked runs fn holding the migration lock, with schema_migrations made.
func (m *Migrator) locked(ctx context.Context, fn func(applied map[string]bool) error) error {
	if _, err := m.Conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, lockKey); err != nil {
		return fmt.Errorf("take the migration lock: %w", err)
	}
	defer m.Conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, lockKey) //nolint:errcheck
	if _, err := m.Conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	rows, err := m.Conn.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("read schema_migrations: %w", err)
	}
	applied := map[string]bool{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		applied[v] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	return fn(applied)
}

// script makes a file one transaction that also runs record: before the
// file's own last COMMIT, or wrapped in a transaction of its own.
func script(sql, record string) string {
	if at := strings.LastIndex(strings.ToUpper(sql), "COMMIT;"); at >= 0 {
		return sql[:at] + record + "\n" + sql[at:]
	}
	return "BEGIN;\n" + sql + "\n" + record + "\nCOMMIT;"
}

// run sends a whole script in one message. A failure leaves its
// transaction aborted, so it is rolled back before the error returns.
func (m *Migrator) run(ctx context.Context, sql string) error {
	if _, err := m.Conn.Exec(ctx, sql); err != nil {
		_, _ = m.Conn.Exec(context.Background(), "ROLLBACK")
		return err
	}
	return nil
}

// ApplyUp applies every pending migration in order and returns their names.
// A failing file stops the run with nothing of it applied or recorded.
func (m *Migrator) ApplyUp(ctx context.Context) ([]string, error) {
	var done []string
	err := m.locked(ctx, func(applied map[string]bool) error {
		names, err := files(m.Up, ".")
		if err != nil {
			return err
		}
		for _, name := range names {
			if applied[name] {
				continue
			}
			body, err := fs.ReadFile(m.Up, name)
			if err != nil {
				return err
			}
			record := fmt.Sprintf("INSERT INTO schema_migrations (version) VALUES ('%s');", name)
			if err := m.run(ctx, script(string(body), record)); err != nil {
				return fmt.Errorf("apply %s (rolled back, not recorded): %w", name, err)
			}
			m.say("applied %s", name)
			done = append(done, name)
		}
		return nil
	})
	return done, err
}

// Rollback undoes the newest steps applied migrations, newest first, each
// in one transaction with its record removed. It refuses up front when
// one of them has no rollback file.
func (m *Migrator) Rollback(ctx context.Context, steps int) ([]string, error) {
	if steps < 1 {
		return nil, errors.New("roll back at least one migration")
	}
	var undone []string
	err := m.locked(ctx, func(applied map[string]bool) error {
		names, err := files(m.Up, ".")
		if err != nil {
			return err
		}
		var newest []string
		for i := len(names) - 1; i >= 0 && len(newest) < steps; i-- {
			if applied[names[i]] {
				newest = append(newest, names[i])
			}
		}
		for _, name := range newest {
			if _, err := fs.Stat(m.Down, "down/"+name); err != nil {
				return fmt.Errorf("%s has no rollback file; restore a backup instead", name)
			}
		}
		for _, name := range newest {
			body, err := fs.ReadFile(m.Down, "down/"+name)
			if err != nil {
				return err
			}
			record := fmt.Sprintf("DELETE FROM schema_migrations WHERE version = '%s';", name)
			if err := m.run(ctx, script(string(body), record)); err != nil {
				return fmt.Errorf("roll back %s (nothing changed): %w", name, err)
			}
			m.say("rolled back %s", name)
			undone = append(undone, name)
		}
		return nil
	})
	return undone, err
}

// Baseline marks every migration up to number as applied without running
// it: for a database migrated before schema_migrations existed.
func (m *Migrator) Baseline(ctx context.Context, number string) error {
	limit, err := strconv.Atoi(number)
	if err != nil || limit < 1 {
		return fmt.Errorf("baseline must be a migration number such as 000006")
	}
	return m.locked(ctx, func(map[string]bool) error {
		names, err := files(m.Up, ".")
		if err != nil {
			return err
		}
		for _, name := range names {
			n, err := strconv.Atoi(name[:6])
			if err != nil || n > limit {
				continue
			}
			if _, err := m.Conn.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1) ON CONFLICT DO NOTHING`, name); err != nil {
				return err
			}
		}
		return nil
	})
}

// Status lists every migration, whether it is applied and whether it can
// be rolled back.
func (m *Migrator) Status(ctx context.Context) ([]Status, error) {
	var out []Status
	err := m.locked(ctx, func(applied map[string]bool) error {
		names, err := files(m.Up, ".")
		if err != nil {
			return err
		}
		for _, name := range names {
			_, statErr := fs.Stat(m.Down, "down/"+name)
			out = append(out, Status{Name: name, Applied: applied[name], HasDown: statErr == nil})
		}
		return nil
	})
	return out, err
}

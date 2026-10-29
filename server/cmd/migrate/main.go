// Command migrate applies the schema migrations carried in the binary to
// the database at DATABASE_URL:
//
//	migrate status          every migration, applied or not, and whether it can be rolled back
//	migrate up              apply the pending ones, in order
//	migrate down N          roll back the newest N applied (only those with a rollback file)
//	migrate baseline 000006 mark migrations up to 000006 applied without running them
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sanjeev0303/object-storage-platform/internal/platform/migrate"
	"github.com/sanjeev0303/object-storage-platform/migrations"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: migrate status | up | down N | baseline NUMBER")
	}
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return fmt.Errorf("DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(context.Background())
	m := &migrate.Migrator{Conn: conn, Up: migrations.Up, Down: migrations.Down, Log: func(line string) { fmt.Println(line) }}

	switch args[0] {
	case "status":
		list, err := m.Status(ctx)
		if err != nil {
			return err
		}
		for _, s := range list {
			state, back := "pending", ""
			if s.Applied {
				state = "applied"
			}
			if s.HasDown {
				back = "  (can roll back)"
			}
			fmt.Printf("%-8s %s%s\n", state, s.Name, back)
		}
	case "up":
		done, err := m.ApplyUp(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("%d applied\n", len(done))
	case "down":
		if len(args) != 2 {
			return fmt.Errorf("usage: migrate down N")
		}
		steps, err := strconv.Atoi(args[1])
		if err != nil {
			return fmt.Errorf("N must be a number")
		}
		undone, err := m.Rollback(ctx, steps)
		if err != nil {
			return err
		}
		fmt.Printf("%d rolled back\n", len(undone))
	case "baseline":
		if len(args) != 2 {
			return fmt.Errorf("usage: migrate baseline NUMBER")
		}
		return m.Baseline(ctx, args[1])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
	return nil
}

// Command event-admin is the operator's tool for the event pipeline: it
// shows outbox, inbox and dead-letter records, and retries, replays,
// resolves or discards dead letters. Every change keeps the -actor and
// -reason given. Event payloads are never printed.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/config"
	"github.com/sanjeev0303/object-storage-platform/internal/service/events"
)

const usage = `usage: event-admin <command> [flags]

commands:
  outbox         list outbox events (-status PENDING|PUBLISHED|DEAD)
  inbox          list consumer records (-consumer, -status)
  dead-letters   list dead letters (-status OPEN|RETRIED|REPLAYED|RESOLVED|DISCARDED)
  inspect        show one dead letter with its event and inbox record (-id)
  retry          send a dead-lettered event again under its own ID (-id -reason)
  replay         send it again under a new ID, as a new operation (-id -reason)
  resolve        close a dead letter handled another way (-id -reason)
  discard        close a dead letter that is dropped on purpose (-id -reason)
  discrepancies  list what event reconciliation found and did
  stats          outbox and dead-letter counts
  emit           write a test event to the outbox (development only)

Run "event-admin <command> -h" for a command's flags.`

func main() {
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "help" {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "event-admin:", err)
		os.Exit(1)
	}
}

func run(command string, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
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

	outbox := postgres.NewOutboxRepository(db)
	inbox := postgres.NewInboxRepository(db)
	dead := postgres.NewDeadLetterRepository(db)
	admin := events.NewAdmin(outbox, inbox, dead, events.Checks{
		Uploads: postgres.NewUploadRepository(db), Assets: postgres.NewAssetRepository(db),
		Jobs: postgres.NewProcessingJobRepository(db), Deletions: postgres.NewDeletionRepository(db),
		Holds: postgres.NewLegalHoldRepository(db), Ingestion: postgres.NewIngestionRepository(db),
		Datasets: postgres.NewDatasetRepository(db),
	})

	flags := flag.NewFlagSet(command, flag.ExitOnError)
	id := flags.String("id", "", "dead letter ID")
	status := flags.String("status", "", "only records with this status")
	limit := flags.Int("limit", 50, "maximum rows to list")
	actor := flags.String("actor", defaultActor(), "who is acting (kept with the dead letter)")
	reason := flags.String("reason", "", "why (required for every change)")

	switch command {
	case "outbox":
		_ = flags.Parse(args)
		return printResult(outbox.List(ctx, strings.ToUpper(*status), *limit))

	case "inbox":
		consumer := flags.String("consumer", "", "processing or lifecycle (default: both)")
		_ = flags.Parse(args)
		return printResult(inbox.List(ctx, *consumer, strings.ToUpper(*status), *limit))

	case "dead-letters":
		_ = flags.Parse(args)
		return printResult(dead.List(ctx, strings.ToUpper(*status), *limit))

	case "inspect":
		_ = flags.Parse(args)
		return printResult(admin.Inspect(ctx, *id))

	case "retry":
		_ = flags.Parse(args)
		if err := admin.Retry(ctx, *id, *actor, *reason); err != nil {
			return explain(err)
		}
		return printJSON(map[string]string{"retried": *id})

	case "replay":
		_ = flags.Parse(args)
		eventID, err := admin.Replay(ctx, *id, *actor, *reason)
		if err != nil {
			return explain(err)
		}
		return printJSON(map[string]string{"replayed": *id, "new_event_id": eventID})

	case "resolve":
		_ = flags.Parse(args)
		if err := admin.Resolve(ctx, *id, *actor, *reason); err != nil {
			return explain(err)
		}
		return printJSON(map[string]string{"resolved": *id})

	case "discard":
		_ = flags.Parse(args)
		if err := admin.Discard(ctx, *id, *actor, *reason); err != nil {
			return explain(err)
		}
		return printJSON(map[string]string{"discarded": *id})

	case "discrepancies":
		_ = flags.Parse(args)
		return printResult(postgres.NewDiscrepancyRepository(db).List(ctx, *limit))

	case "stats":
		_ = flags.Parse(args)
		now := time.Now().UTC()
		outboxStats, err := outbox.Stats(ctx, now)
		if err != nil {
			return err
		}
		deadStats, err := dead.Stats(ctx, now)
		if err != nil {
			return err
		}
		return printJSON(map[string]any{"outbox": outboxStats, "dead_letters": deadStats})

	case "emit":
		eventType := flags.String("type", "", "event type, e.g. upload.completed")
		tenant := flags.String("tenant", "", "tenant ID")
		aggregateType := flags.String("aggregate-type", "", "aggregate type, e.g. upload")
		aggregateID := flags.String("aggregate-id", "", "aggregate ID")
		payload := flags.String("payload", "{}", "JSON payload: identifiers only")
		queue := flags.String("queue", "", "destination queue (default: the type's queue)")
		version := flags.Int("version", 1, "schema version")
		_ = flags.Parse(args)
		if cfg.AppEnv != "development" {
			return errors.New("emit writes arbitrary events and is refused unless APP_ENV=development")
		}
		return emit(ctx, outbox, *eventType, *tenant, *aggregateType, *aggregateID, *payload, *queue, *version)
	}
	return fmt.Errorf("unknown command %q\n\n%s", command, usage)
}

// emit writes a test event straight to the outbox, so the publisher and a
// consumer handle it like any other. An unknown type or version, or a
// payload naming a record that does not exist, shows the dead-letter path.
func emit(ctx context.Context, outbox *postgres.OutboxRepository, eventType, tenant, aggregateType, aggregateID, payload, queue string, version int) error {
	if eventType == "" || tenant == "" || aggregateType == "" || aggregateID == "" {
		return errors.New("-type, -tenant, -aggregate-type and -aggregate-id are required")
	}
	if !json.Valid([]byte(payload)) {
		return errors.New("-payload must be valid JSON")
	}
	if queue == "" {
		known, ok := event.QueueOf(eventType)
		if !ok {
			return fmt.Errorf("%s is not a known type: name its -queue", eventType)
		}
		queue = known
	}

	now := time.Now().UTC()
	e := model.OutboxEvent{
		ID: event.NewID(), Type: eventType, SchemaVersion: version,
		AggregateType: aggregateType, AggregateID: aggregateID, TenantID: tenant,
		Payload: []byte(payload), CorrelationID: "event-admin-" + event.NewID()[:8], Destination: queue,
		Status: model.OutboxPending, NextAttemptAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := outbox.Append(ctx, e); err != nil {
		return err
	}
	return printJSON(map[string]string{"event_id": e.ID, "queue": queue, "correlation_id": e.CorrelationID})
}

// explain adds what an operator can do next to the refusals they can hit.
func explain(err error) error {
	switch {
	case errors.Is(err, model.ErrStaleEvent):
		return fmt.Errorf("%w\nthe business state moved on: check it with lifecycle-admin or processing-worker -inspect", err)
	case errors.Is(err, model.ErrNotReplayable):
		return fmt.Errorf("%w: the outbox no longer has the event (an unreadable message, or compacted)", err)
	case errors.Is(err, model.ErrDeadLetterClosed):
		return fmt.Errorf("%w: see its resolution with inspect", err)
	}
	return err
}

func printResult[T any](value T, err error) error {
	if err != nil {
		return err
	}
	return printJSON(value)
}

func printJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func defaultActor() string {
	for _, key := range []string{"USERNAME", "USER"} {
		if name := os.Getenv(key); name != "" {
			return name
		}
	}
	return "operator"
}

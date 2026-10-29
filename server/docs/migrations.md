# Schema migrations

The schema is the files in `migrations/`, applied in name order and recorded in `schema_migrations`. Two tools apply them with the same guarantees:

- **`cmd/migrate`** (production, CI, any machine): the files are embedded in the binary, and it connects to `DATABASE_URL`.
- **`scripts/migrate.ps1`** (local Windows): runs `psql` inside the Docker Compose PostgreSQL.

Both take the same advisory lock, so two runs never interleave. Each file is applied and recorded in one transaction: a failing file leaves nothing applied and nothing recorded, and the run stops there.

## Commands

```sh
DATABASE_URL=postgres://... migrate status        # every migration: applied or pending, and whether it can roll back
DATABASE_URL=postgres://... migrate up            # apply the pending ones, in order
DATABASE_URL=postgres://... migrate down 2        # roll back the newest 2 (only those with a rollback file)
DATABASE_URL=postgres://... migrate baseline 6    # mark 000001..000006 applied without running them
```

## Rules for a new migration

- **Additive first (expand, then contract):** add tables, columns and constraints that the running version ignores. Remove old ones in a later release, once no running version uses them. A deploy then never needs the schema and the code to change at the same instant.
- **Re-runnable:** use `IF NOT EXISTS` and `IF EXISTS`, so a file applied by hand once is not an error the next time.
- **One transaction:** nothing that cannot run in a transaction (`CREATE INDEX CONCURRENTLY`, `VACUUM`, `ALTER TYPE ... ADD VALUE`). An index on a large table gets its own release, created concurrently by hand and then recorded with `baseline`.
- **A rollback file:** each release's new migrations get `migrations/down/<same name>.sql`, which undoes them (dropping what they added, and saying in a comment what data that loses). Only the newest release keeps them. Older ones are rolled back by restoring a backup.

## Releasing

1. Take a backup or a point-in-time recovery mark of the database.
2. Run `migrate status` against production and check that the pending files are exactly this release's.
3. Run `migrate up` as a one-off task (an ECS task or Kubernetes Job) before the services roll out. The old services keep working on the expanded schema.
4. Roll the services out.

**If the release must be undone:**
1. Roll the services back.
2. Run `migrate down N` for this release's N migrations, if the old code cannot live with the new tables.
3. If a migration without a rollback file is involved, restore the backup from step 1.

## Rehearsal

`TestMigrationsFromScratchAndRollback` (integration) runs on every test run:
- applies every migration to an empty database, then again (nothing to do);
- compares the resulting tables, columns, indexes, constraints and triggers with the test database, which was migrated file by file over time;
- rolls back the newest release and checks that rolling back past it is refused;
- migrates forward again, to the same schema.

Last result (2026-10-29): 28 migrations, schemas identical, rollback of 000027 and 000028 and forward again clean.

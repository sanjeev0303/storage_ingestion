package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// TransactionManager runs a group of dataabse operations automically
type TransactionManager struct {
	db *DB
}

func NewTransactionManager(db *DB) *TransactionManager {
	return &TransactionManager{db: db}
}

// WithinTransaxtion commits when fn succeeds and rolls back on failure.
func (m *TransactionManager) WithinTransaction(
	ctx context.Context,
	fn func(pgx.Tx) error,
) error {
	tx, err := m.db.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	// Rollback is harmless after a successful commit and is a useful
	// safeguard if the callback of commit returns an error.
	defer func() {
		_ = tx.Rollback(context.Background())
	}()

	if err := fn(tx); err != nil {
		return fmt.Errorf("transaction operations: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	return nil
}

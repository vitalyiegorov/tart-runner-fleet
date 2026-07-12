package sqlite

import (
	"context"
	"database/sql"
)

type rowsScanner interface {
	Next() bool
	Scan(...any) error
	Close() error
	Err() error
}

type errorScanner struct{ err error }

func (s errorScanner) Scan(...any) error { return s.err }

func (s *Store) faultAt(point string) error {
	if s.injectFault == nil {
		return nil
	}
	return s.injectFault(point)
}

func (s *Store) beginTx(ctx context.Context, point string) (*sql.Tx, error) {
	if err := s.faultAt(point); err != nil {
		return nil, err
	}
	return s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
}

func (s *Store) dbExec(ctx context.Context, point, query string, args ...any) (sql.Result, error) {
	if err := s.faultAt(point); err != nil {
		return nil, err
	}
	return s.db.ExecContext(ctx, query, args...)
}

func (s *Store) txExec(ctx context.Context, tx *sql.Tx, point, query string, args ...any) (sql.Result, error) {
	if err := s.faultAt(point); err != nil {
		return nil, err
	}
	return tx.ExecContext(ctx, query, args...)
}

func (s *Store) dbRow(ctx context.Context, point, query string, args ...any) rowScanner {
	if err := s.faultAt(point); err != nil {
		return errorScanner{err: err}
	}
	return s.db.QueryRowContext(ctx, query, args...)
}

func (s *Store) txRow(ctx context.Context, tx *sql.Tx, point, query string, args ...any) rowScanner {
	if err := s.faultAt(point); err != nil {
		return errorScanner{err: err}
	}
	return tx.QueryRowContext(ctx, query, args...)
}

func (s *Store) dbQuery(ctx context.Context, point, query string, args ...any) (rowsScanner, error) {
	if err := s.faultAt(point); err != nil {
		return nil, err
	}
	if s.injectRows != nil {
		if rows := s.injectRows(point); rows != nil {
			return rows, nil
		}
	}
	return s.db.QueryContext(ctx, query, args...)
}

func (s *Store) txQuery(ctx context.Context, tx *sql.Tx, point, query string, args ...any) (rowsScanner, error) {
	if err := s.faultAt(point); err != nil {
		return nil, err
	}
	if s.injectRows != nil {
		if rows := s.injectRows(point); rows != nil {
			return rows, nil
		}
	}
	return tx.QueryContext(ctx, query, args...)
}

func (s *Store) commit(tx *sql.Tx, point string) error {
	if err := s.faultAt(point); err != nil {
		return err
	}
	return tx.Commit()
}

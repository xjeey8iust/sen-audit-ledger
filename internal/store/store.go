// Package store owns the SQLite file and every write the service performs.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sync"

	_ "modernc.org/sqlite"
)

// Store wraps the SQLite handle so callers never touch database/sql directly.
type Store struct {
	db *sql.DB
	mu sync.Mutex // serializes appends so the chain has a single writer
}

// Open prepares the database file and the schema this service needs.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable wal: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Ping reports whether the storage layer is usable.
func (s *Store) Ping() error { return s.db.Ping() }

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// EventInput carries the validated business fields of one audit record. Seq is
// nil when the caller lets the service assign the next sequence number.
type EventInput struct {
	Account    string
	Operation  string
	Resource   string
	Result     string
	OccurredAt string
	Seq        *uint64
}

// Event is one persisted audit record together with its chain links.
type Event struct {
	Seq        uint64
	Account    string
	Operation  string
	Resource   string
	Result     string
	OccurredAt string
	PrevHash   string
	Hash       string
}

// ErrSeqConflict reports an explicit seq that already exists in the ledger.
var ErrSeqConflict = errors.New("seq already recorded")

// ErrSeqNotNext reports an explicit seq that is neither recorded nor the next
// sequence number the ledger would assign.
var ErrSeqNotNext = errors.New("seq is not the next sequence number")

// Append persists one audit event. The tail lookup, sequence assignment and
// insert happen inside a single transaction, and the mutex keeps one writer at
// a time so concurrent appends commit in a gap-free chain order. A failed
// append rolls back and therefore neither consumes a sequence number nor moves
// the chain tail.
func (s *Store) Append(in EventInput) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return Event{}, fmt.Errorf("begin append: %w", err)
	}
	defer tx.Rollback()

	tailSeq := uint64(0)
	tailHash := zeroPrevHash
	err = tx.QueryRow(`SELECT seq, hash FROM audit_events ORDER BY seq DESC LIMIT 1`).Scan(&tailSeq, &tailHash)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return Event{}, fmt.Errorf("read chain tail: %w", err)
	}

	seq := tailSeq + 1
	if in.Seq != nil {
		switch {
		case *in.Seq <= tailSeq:
			return Event{}, ErrSeqConflict
		case *in.Seq != seq:
			return Event{}, ErrSeqNotNext
		}
	}

	event := Event{
		Seq:        seq,
		Account:    in.Account,
		Operation:  in.Operation,
		Resource:   in.Resource,
		Result:     in.Result,
		OccurredAt: in.OccurredAt,
		PrevHash:   tailHash,
	}
	event.Hash = chainHash(event.Seq, event.Account, event.Operation, event.Resource, event.Result, event.OccurredAt, event.PrevHash)

	if _, err := tx.Exec(`INSERT INTO audit_events (seq, account, operation, resource, result, occurred_at, prev_hash, hash) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		event.Seq, event.Account, event.Operation, event.Resource, event.Result, event.OccurredAt, event.PrevHash, event.Hash); err != nil {
		return Event{}, fmt.Errorf("insert event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Event{}, fmt.Errorf("commit append: %w", err)
	}
	return event, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS service_metadata (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS audit_events (
	seq         INTEGER PRIMARY KEY,
	account     TEXT NOT NULL,
	operation   TEXT NOT NULL,
	resource    TEXT NOT NULL,
	result      TEXT NOT NULL,
	occurred_at TEXT NOT NULL,
	prev_hash   TEXT NOT NULL,
	hash        TEXT NOT NULL
);
`

// Package store owns the SQLite file and every write the service performs.
package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

// Event is one persisted audit record.
type Event struct {
	Seq        int64
	Account    string
	Operation  string
	Resource   string
	Result     string
	OccurredAt string
	PrevHash   string
	Hash       string
}

// EventInput carries the validated fields an append needs. Seq is nil when the
// caller lets the service assign the next sequence number.
type EventInput struct {
	Seq        *int64
	Account    string
	Operation  string
	Resource   string
	Result     string
	OccurredAt string
}

// Append failures the API maps to specific status codes.
var (
	ErrSeqConflict = errors.New("seq already exists in the ledger")
	ErrSeqGap      = errors.New("seq is not the next sequence number")
)

// genesisPrevHash is the prev_hash of the first record in the chain.
const genesisPrevHash = "0000000000000000000000000000000000000000000000000000000000000000"

// Store wraps the SQLite handle so callers never touch database/sql directly.
type Store struct {
	db *sql.DB
	// mu serializes appends: the chain tail is read and extended inside one
	// transaction, so a single writer at a time keeps seq gap-free.
	mu sync.Mutex
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

// Append validates the sequence position against the current chain tail and
// persists one record inside a single transaction. A failed append consumes no
// sequence number and leaves the chain tail untouched.
func (s *Store) Append(in EventInput) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return Event{}, fmt.Errorf("begin append: %w", err)
	}
	defer tx.Rollback()

	var lastSeq int64
	lastHash := genesisPrevHash
	row := tx.QueryRow("SELECT seq, hash FROM events ORDER BY seq DESC LIMIT 1")
	if err := row.Scan(&lastSeq, &lastHash); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Event{}, fmt.Errorf("read chain tail: %w", err)
	}

	next := lastSeq + 1
	seq := next
	if in.Seq != nil {
		switch {
		case *in.Seq <= lastSeq:
			return Event{}, ErrSeqConflict
		case *in.Seq != next:
			return Event{}, ErrSeqGap
		}
	}

	event := Event{
		Seq:        seq,
		Account:    in.Account,
		Operation:  in.Operation,
		Resource:   in.Resource,
		Result:     in.Result,
		OccurredAt: in.OccurredAt,
		PrevHash:   lastHash,
	}
	event.Hash = computeHash(event)

	if _, err := tx.Exec(
		"INSERT INTO events (seq, account, operation, resource, result, occurred_at, prev_hash, hash) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		event.Seq, event.Account, event.Operation, event.Resource, event.Result, event.OccurredAt, event.PrevHash, event.Hash,
	); err != nil {
		return Event{}, fmt.Errorf("insert event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Event{}, fmt.Errorf("commit append: %w", err)
	}
	return event, nil
}

// VerifyResult reports the outcome of a full ledger chain check. Checked
// counts every record examined, including the one that triggered a failure.
// FirstInvalidSeq is nil when the chain is valid; on a sequence break it
// holds the expected (missing) seq, on a linkage or hash mismatch it holds
// the offending record's own seq.
type VerifyResult struct {
	Valid           bool
	Checked         int64
	FirstInvalidSeq *int64
}

// Verify walks the whole ledger in ascending seq order against one committed
// snapshot: a single read transaction, so appends committing mid-check are
// seen either fully before or fully after, never mixed. It confirms seqs
// increment contiguously from 1, that each prev_hash links to the previous
// stored hash (genesis for the first record), and that each stored hash
// matches a recomputation from the stored fields. The walk stops at the
// first violation and never writes back.
func (s *Store) Verify() (VerifyResult, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return VerifyResult{}, fmt.Errorf("begin verify: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.Query("SELECT seq, account, operation, resource, result, occurred_at, prev_hash, hash FROM events ORDER BY seq ASC")
	if err != nil {
		return VerifyResult{}, fmt.Errorf("read ledger: %w", err)
	}
	defer rows.Close()

	result := VerifyResult{Valid: true}
	var expectedSeq int64 = 1
	prevHash := genesisPrevHash
	for rows.Next() {
		var event Event
		if err := rows.Scan(&event.Seq, &event.Account, &event.Operation, &event.Resource,
			&event.Result, &event.OccurredAt, &event.PrevHash, &event.Hash); err != nil {
			return VerifyResult{}, fmt.Errorf("scan event: %w", err)
		}
		result.Checked++
		if event.Seq != expectedSeq {
			result.Valid = false
			seq := expectedSeq
			result.FirstInvalidSeq = &seq
			return result, nil
		}
		if event.PrevHash != prevHash || computeHash(event) != event.Hash {
			result.Valid = false
			seq := event.Seq
			result.FirstInvalidSeq = &seq
			return result, nil
		}
		prevHash = event.Hash
		expectedSeq++
	}
	if err := rows.Err(); err != nil {
		return VerifyResult{}, fmt.Errorf("iterate ledger: %w", err)
	}
	return result, nil
}

// computeHash hashes the compact JSON array
// [seq, account, operation, resource, result, occurred_at, prev_hash] encoded
// as UTF-8. Strings escape only the double quote, the backslash and control
// characters (as lowercase \u00xx); everything else stays raw.
func computeHash(e Event) string {
	var b strings.Builder
	b.WriteByte('[')
	b.WriteString(strconv.FormatInt(e.Seq, 10))
	for _, field := range []string{e.Account, e.Operation, e.Resource, e.Result, e.OccurredAt, e.PrevHash} {
		b.WriteByte(',')
		writeEscaped(&b, field)
	}
	b.WriteByte(']')
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func writeEscaped(b *strings.Builder, s string) {
	const hexdigits = "0123456789abcdef"
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < 0x20:
			b.WriteString(`\u00`)
			b.WriteByte(hexdigits[c>>4])
			b.WriteByte(hexdigits[c&0x0f])
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
}

const schema = `
CREATE TABLE IF NOT EXISTS service_metadata (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS events (
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

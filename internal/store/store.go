// Package store owns the SQLite file and every write the service performs.
package store

import (
	"context"
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

// VerifyResult reports one full-ledger verification pass.
type VerifyResult struct {
	// Valid is true when the ledger is empty or every record read from the
	// snapshot forms one continuous, correctly hashed chain.
	Valid bool
	// Checked is the number of records examined, including the record that
	// triggered the first failure.
	Checked int64
	// FirstInvalidSeq is the sequence position of the first broken record, or
	// nil when Valid is true. A seq mismatch returns the expected seq; a chain
	// or hash mismatch returns the stored record's own seq.
	FirstInvalidSeq *int64
}

// VerifyChain reads the whole ledger in one read-only transaction so the pass
// sees a single committed snapshot; an append landing during the check yields
// either the pre-append or the post-append chain, never a mix. Records are
// examined in ascending seq, stopping at the first record that is not the next
// expected sequence, does not chain to the previous record's stored hash, or
// does not hash to its stored value. Verification never mutates records.
func (s *Store) VerifyChain() (VerifyResult, error) {
	tx, err := s.db.BeginTx(context.TODO(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return VerifyResult{}, fmt.Errorf("begin verify: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.Query(
		"SELECT seq, account, operation, resource, result, occurred_at, prev_hash, hash FROM events ORDER BY seq ASC",
	)
	if err != nil {
		return VerifyResult{}, fmt.Errorf("read ledger: %w", err)
	}
	defer rows.Close()

	var expected int64 = 1
	prevHash := genesisPrevHash
	var checked int64
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.Seq, &e.Account, &e.Operation, &e.Resource, &e.Result, &e.OccurredAt, &e.PrevHash, &e.Hash); err != nil {
			return VerifyResult{}, fmt.Errorf("scan event: %w", err)
		}
		checked++
		if e.Seq != expected {
			return VerifyResult{Valid: false, Checked: checked, FirstInvalidSeq: &expected}, nil
		}
		if e.PrevHash != prevHash || computeHash(e) != e.Hash {
			return VerifyResult{Valid: false, Checked: checked, FirstInvalidSeq: &e.Seq}, nil
		}
		prevHash = e.Hash
		expected++
	}
	if err := rows.Err(); err != nil {
		return VerifyResult{}, fmt.Errorf("iterate ledger: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return VerifyResult{}, fmt.Errorf("commit verify: %w", err)
	}
	return VerifyResult{Valid: true, Checked: checked}, nil
}

// EventFilter carries the validated bounds of one GET /events page. From and
// To are normalized UTC timestamps in the same form records store; HasFrom
// and HasTo mark whether each bound applies. A nil Account matches every
// account.
type EventFilter struct {
	Account  *string
	From     string
	HasFrom  bool
	To       string
	HasTo    bool
	AfterSeq int64
	Limit    int
}

// ListEvents returns up to Limit records matching the filter in ascending
// seq, plus whether further matching records exist beyond the page. The read
// runs inside one read-only transaction, so the page reflects a single
// committed view of the ledger; records appended afterwards only show up in
// later requests. Listing never mutates records.
func (s *Store) ListEvents(f EventFilter) ([]Event, bool, error) {
	tx, err := s.db.BeginTx(context.TODO(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, false, fmt.Errorf("begin list: %w", err)
	}
	defer tx.Rollback()

	query := "SELECT seq, account, operation, resource, result, occurred_at, prev_hash, hash FROM events WHERE seq > ?"
	args := []any{f.AfterSeq}
	if f.Account != nil {
		// TEXT equality uses the binary collation: case-sensitive and exact,
		// so the stored text must match the filter byte for byte.
		query += " AND account = ?"
		args = append(args, *f.Account)
	}
	query += " ORDER BY seq ASC"

	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("query events: %w", err)
	}
	defer rows.Close()

	events := make([]Event, 0, f.Limit)
	hasMore := false
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.Seq, &e.Account, &e.Operation, &e.Resource, &e.Result, &e.OccurredAt, &e.PrevHash, &e.Hash); err != nil {
			return nil, false, fmt.Errorf("scan event: %w", err)
		}
		// occurred_at is compared as an instant, not as text: fractions of
		// any length participate and equal instants written differently
		// (".5" vs ".50") must both fall inside or outside the window.
		if f.HasFrom && CompareInstants(e.OccurredAt, f.From) < 0 {
			continue
		}
		if f.HasTo && CompareInstants(e.OccurredAt, f.To) >= 0 {
			continue
		}
		if len(events) == f.Limit {
			// One more matching record exists beyond this page.
			hasMore = true
			break
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("iterate events: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("commit list: %w", err)
	}
	return events, hasMore, nil
}

// CompareInstants compares two normalized UTC timestamps (fixed-width head,
// optional fraction of any length, trailing Z) by the instant they denote,
// returning -1, 0 or 1. Fractions are compared as decimal values, so trailing
// zeros carry no weight and different writings of one instant compare equal.
func CompareInstants(a, b string) int {
	aHead, aFraction := splitInstant(a)
	bHead, bFraction := splitInstant(b)
	if aHead != bHead {
		if aHead < bHead {
			return -1
		}
		return 1
	}
	if len(aFraction) < len(bFraction) {
		aFraction += strings.Repeat("0", len(bFraction)-len(aFraction))
	} else if len(bFraction) < len(aFraction) {
		bFraction += strings.Repeat("0", len(aFraction)-len(bFraction))
	}
	return strings.Compare(aFraction, bFraction)
}

// splitInstant separates a normalized UTC timestamp into its fixed-width
// second head and its fraction digits (empty when there is no fraction).
func splitInstant(s string) (head, fraction string) {
	const headLen = len("2006-01-02T15:04:05")
	if len(s) > headLen+1 && s[headLen] == '.' {
		return s[:headLen], s[headLen+1 : len(s)-1]
	}
	return s[:headLen], ""
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

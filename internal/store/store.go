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
	"time"

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
		// Test seam: an installed row hook observes each scanned row before
		// the chain checks run. Always nil in the service; see verify_hook.go.
		if hook := verifyRowHook.Load(); hook != nil {
			(*hook)(e.Seq)
		}
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

// EventFilter narrows the records ListEvents returns. Account and the time
// bounds are optional: a nil pointer applies no constraint. From and To must
// already be normalized UTC timestamps in the stored occurred_at format.
type EventFilter struct {
	Account  *string // exact, case-sensitive match on the stored text
	From     *string // inclusive lower bound on the instant
	To       *string // exclusive upper bound on the instant
	AfterSeq int64   // only records with a seq greater than this
	Limit    int64   // page size; ListEvents reads at most Limit+1 matches
}

// ListEvents reads one page of the ledger in ascending seq inside a single
// read-only transaction, so the page reflects one committed view; records
// appended afterwards are visible to later pages, never to this one. At most
// Limit+1 matching records are returned: a result longer than Limit means
// more matches exist past the page. When a time window applies, every
// occurred_at the scan reads must be a UTC value in the shape the append
// path persists; anything else is a storage anomaly and fails the whole
// read rather than being skipped or compared leniently. Listing never
// mutates records.
func (s *Store) ListEvents(f EventFilter) ([]Event, error) {
	tx, err := s.db.BeginTx(context.TODO(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin list: %w", err)
	}
	defer tx.Rollback()

	query := "SELECT seq, account, operation, resource, result, occurred_at, prev_hash, hash FROM events WHERE seq > ?"
	args := []any{f.AfterSeq}
	if f.Account != nil {
		query += " AND account = ?"
		args = append(args, *f.Account)
	}
	query += " ORDER BY seq ASC"

	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query events: %w", err)
	}
	defer rows.Close()

	events := []Event{}
	for rows.Next() && int64(len(events)) <= f.Limit {
		var e Event
		if err := rows.Scan(&e.Seq, &e.Account, &e.Operation, &e.Resource, &e.Result, &e.OccurredAt, &e.PrevHash, &e.Hash); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		if (f.From != nil || f.To != nil) && !isStoredInstant(e.OccurredAt) {
			// A windowed query compares instants, so every occurred_at it
			// reads must be a UTC value in the shape the append path
			// persists. Anything else is a storage anomaly, not a record
			// to skip or to compare leniently.
			return nil, fmt.Errorf("stored occurred_at is not a persisted UTC instant")
		}
		if f.From != nil && CompareInstants(e.OccurredAt, *f.From) < 0 {
			continue
		}
		if f.To != nil && CompareInstants(e.OccurredAt, *f.To) >= 0 {
			continue
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate events: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close events: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit list: %w", err)
	}
	return events, nil
}

// CompareInstants orders two normalized UTC occurred_at values by the actual
// instant they denote. Fractional seconds of any length participate: the
// shorter fraction is right-padded with zeros, so ".5" and ".50" are equal
// while ".5001" sorts after both. Different spellings of one instant compare
// equal because both values are already normalized to UTC.
func CompareInstants(a, b string) int {
	aSec, bSec := secondPrefix(a), secondPrefix(b)
	if aSec != bSec {
		if aSec < bSec {
			return -1
		}
		return 1
	}
	aFrac, bFrac := fractionDigits(a), fractionDigits(b)
	if len(aFrac) < len(bFrac) {
		aFrac += strings.Repeat("0", len(bFrac)-len(aFrac))
	} else {
		bFrac += strings.Repeat("0", len(aFrac)-len(bFrac))
	}
	return strings.Compare(aFrac, bFrac)
}

// isStoredInstant reports whether value is a UTC occurred_at in the exact
// shape the append path persists: a valid zero-padded
// "YYYY-MM-DDTHH:MM:SS" head, an optional fractional second of one or more
// digits, and a trailing "Z". Anything else — empty, truncated, missing the
// zone, an impossible date or a non-numeric fraction — can never have come
// from a validated append.
func isStoredInstant(value string) bool {
	const headLen = len("2006-01-02T15:04:05")
	if len(value) < headLen+1 {
		return false
	}
	head := value[:headLen]
	if head[4] != '-' || head[7] != '-' || head[10] != 'T' || head[13] != ':' || head[16] != ':' {
		return false
	}
	field := func(start, width int) (int, bool) {
		v := 0
		for i := start; i < start+width; i++ {
			if head[i] < '0' || head[i] > '9' {
				return 0, false
			}
			v = v*10 + int(head[i]-'0')
		}
		return v, true
	}
	year, ok1 := field(0, 4)
	month, ok2 := field(5, 2)
	day, ok3 := field(8, 2)
	hour, ok4 := field(11, 2)
	minute, ok5 := field(14, 2)
	second, ok6 := field(17, 2)
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || !ok6 || second > 59 {
		return false
	}
	t := time.Date(year, time.Month(month), day, hour, minute, second, 0, time.UTC)
	if t.Year() != year || int(t.Month()) != month || t.Day() != day || t.Hour() != hour || t.Minute() != minute {
		return false
	}
	rest := value[headLen:]
	if rest == "Z" {
		return true
	}
	if len(rest) < 3 || rest[0] != '.' || rest[len(rest)-1] != 'Z' {
		return false
	}
	for i := 1; i < len(rest)-1; i++ {
		if rest[i] < '0' || rest[i] > '9' {
			return false
		}
	}
	return true
}

// secondPrefix returns the fixed-width "YYYY-MM-DDTHH:MM:SS" head of a
// normalized UTC occurred_at value. The head is zero-padded, so lexicographic
// order on it matches chronological order.
func secondPrefix(value string) string {
	return value[:len("2006-01-02T15:04:05")]
}

// fractionDigits returns the fractional-second digits of a normalized UTC
// occurred_at value, or "" when the value carries no fraction.
func fractionDigits(value string) string {
	rest := value[len("2006-01-02T15:04:05"):]
	if rest == "Z" {
		return ""
	}
	return rest[1 : len(rest)-1] // strip the leading "." and trailing "Z"
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

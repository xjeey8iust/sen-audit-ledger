package store

// Regression tests for a windowed ListEvents reading a record whose stored
// occurred_at is not the canonical UTC shape the append entry produces:
//
//   - with a time window, such a record is a storage error (ErrCorruptRecord)
//     whenever the scan reaches it, even when valid records were read first:
//     the request fails as a whole instead of comparing the malformed text;
//   - without a time window, stored text is returned verbatim and never
//     normalized or rejected;
//   - an account filter or an after_seq cursor that excludes the corrupt
//     row lets the other records page normally;
//   - verification is still governed only by the chain hashes: a corrupt
//     timestamp whose stored hash is self-consistent verifies as valid.
//
// The bad row is inserted through an independent connection with a correct
// chain shape, so this exercises the read path rather than Append.

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// insertCorruptRow writes one out-of-band row with the given occurred_at but
// a prev_hash/hash pair that is internally consistent for that text, so the
// only thing wrong with the row is its timestamp spelling.
func insertCorruptRow(t *testing.T, path string, e Event) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open control connection: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(
		"INSERT INTO events (seq, account, operation, resource, result, occurred_at, prev_hash, hash) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		e.Seq, e.Account, e.Operation, e.Resource, e.Result, e.OccurredAt, e.PrevHash, e.Hash,
	); err != nil {
		t.Fatalf("insert corrupt row: %v", err)
	}
}

func TestListEventsCorruptOccurredAtFailsOnlyInsideWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	good, err := st.Append(EventInput{
		Account: "alice", Operation: "login", Resource: "console", Result: "ok",
		OccurredAt: "2026-10-05T00:30:00Z",
	})
	if err != nil {
		t.Fatalf("append good: %v", err)
	}

	// A short, unparseable timestamp with an otherwise correct chain shape:
	// its hash is recomputed over the exact (bad) text the row stores.
	badOccurredAt := "2026-10-05"
	bad := Event{
		Seq: good.Seq + 1, Account: "bob", Operation: "read", Resource: "report",
		Result: "denied", OccurredAt: badOccurredAt, PrevHash: good.Hash,
	}
	bad.Hash = computeHash(bad)
	insertCorruptRow(t, path, bad)

	from := "2026-10-05T00:00:00Z"
	to := "2026-10-06T00:00:00Z"

	// A window that would contain the row fails as storage corruption once
	// the scan reaches it, even though seq 1 was read and is valid first.
	if _, err := st.ListEvents(EventFilter{To: &to, Limit: 50}); !errors.Is(err, ErrCorruptRecord) {
		t.Fatalf("to-window over corrupt row: err = %v, want ErrCorruptRecord", err)
	}
	if _, err := st.ListEvents(EventFilter{From: &from, Limit: 50}); !errors.Is(err, ErrCorruptRecord) {
		t.Fatalf("from-window over corrupt row: err = %v, want ErrCorruptRecord", err)
	}

	// No time window: the stored (bad) text is returned verbatim, never
	// normalized or rejected.
	events, err := st.ListEvents(EventFilter{Limit: 50})
	if err != nil {
		t.Fatalf("windowless list: %v", err)
	}
	if len(events) != 2 || events[1].OccurredAt != badOccurredAt {
		t.Fatalf("windowless list = %+v, want the corrupt text preserved", events)
	}

	// An account filter that excludes the corrupt row pages normally.
	alice := "alice"
	events, err = st.ListEvents(EventFilter{Account: &alice, To: &to, Limit: 50})
	if err != nil {
		t.Fatalf("account-filtered list: %v", err)
	}
	if len(events) != 1 || events[0].Seq != good.Seq {
		t.Fatalf("account-filtered list = %+v, want only the good alice row", events)
	}

	// A cursor past the corrupt row never scans it and stays healthy.
	events, err = st.ListEvents(EventFilter{AfterSeq: bad.Seq, To: &to, Limit: 50})
	if err != nil {
		t.Fatalf("cursor-past-corruption list: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("cursor-past-corruption list = %+v, want no rows", events)
	}
}

func TestListEventsCorruptRowAfterValidPrefixFailsWholePage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	first, err := st.Append(EventInput{
		Account: "a", Operation: "o", Resource: "r", Result: "ok",
		OccurredAt: "2026-10-05T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("append first: %v", err)
	}
	second, err := st.Append(EventInput{
		Account: "a", Operation: "o", Resource: "r", Result: "ok",
		OccurredAt: "2026-10-05T01:00:00Z",
	})
	if err != nil {
		t.Fatalf("append second: %v", err)
	}

	// Seq 3 keeps a self-consistent hash but a truncated timestamp.
	badText := "2026-10-05T02:00:0"
	third := Event{
		Seq: 3, Account: "a", Operation: "o", Resource: "r", Result: "ok",
		OccurredAt: badText, PrevHash: second.Hash,
	}
	third.Hash = computeHash(third)
	insertCorruptRow(t, path, third)

	to := "2026-10-06T00:00:00Z"
	// limit 2 must read seq 3 (limit+1) to size the page; that read hits
	// the corrupt row and fails the whole request rather than returning the
	// valid first page.
	_, err = st.ListEvents(EventFilter{To: &to, Limit: 2})
	if !errors.Is(err, ErrCorruptRecord) {
		t.Fatalf("limit-2 page over a corrupt third row: err = %v, want ErrCorruptRecord", err)
	}

	// The chain still verifies: hashes and links are self-consistent, and a
	// bad timestamp spelling is itself never a verification failure.
	result, err := st.VerifyChain()
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !result.Valid || result.Checked != 3 || result.FirstInvalidSeq != nil {
		t.Fatalf("verify = %+v, want a valid chain of 3 despite the bad timestamp text", result)
	}

	_ = first
}

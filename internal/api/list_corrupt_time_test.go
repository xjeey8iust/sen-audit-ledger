package api

// Regression tests for GET /events reading a ledger row whose stored
// occurred_at is empty, truncated or otherwise not the canonical UTC shape
// the append entry produces:
//
//   - a request carrying from/to must fail the whole read with 503
//     storage_unavailable once the scan reaches that row, even after a valid
//     prefix was already read: it never returns the first page or a cursor;
//   - the malformed shapes covered are a short date, a missing zone, an
//     illegal date and a non-numeric fractional second;
//   - an invalid query parameter still wins as 400 invalid_audit_input;
//   - an account filter or after_seq cursor that excludes the bad row pages
//     the remaining records normally;
//   - without a time window the stored text is returned verbatim, never
//     normalized or rejected;
//   - verification is unchanged: the corrupt row is rewritten with a hash
//     consistent with its bad text, so the chain still verifies and no seq
//     is consumed;
//   - after the timestamp and hash are restored, the windowed pages and
//     cursor match the original ledger again.
//
// The bad row is rewritten through the test-only control connection with
// the hash recomputed by the public SHA-256 rule, which keeps every chain
// link self-consistent and isolates the timestamp-spelling defect.

import (
	"net/http"
	"path/filepath"
	"testing"
)

// corruptEventTime rewrites one row's occurred_at to badText and its hash to
// the value consistent with that text, leaving seq, the business fields and
// the back-link intact so the chain still verifies.
func corruptEventTime(t *testing.T, path string, seq int64, prevHash, badText string) {
	t.Helper()
	badHash := wantHash(seq, "a", "op", "res", "ok", badText, prevHash)
	corrupt(t, path, "UPDATE events SET occurred_at = ?, hash = ? WHERE seq = ?", badText, badHash, seq)
}

// seedTwoEvents appends two valid records for account "a" and returns their
// 201 bodies; seq 2 chains onto seq 1.
func seedTwoEvents(t *testing.T, router http.Handler) []map[string]any {
	t.Helper()
	first := mustPost(t, router, eventBody("a", "2026-10-05T00:00:00Z"))
	second := mustPost(t, router, eventBody("a", "2026-10-05T01:00:00Z"))
	return []map[string]any{first, second}
}

func TestGetEventsCorruptOccurredAtFailsWindowedRead(t *testing.T) {
	// Every shape must be rejected by NormalizeOccurredAt yet survives as
	// stored text in a windowless read.
	cases := []string{
		"2026-10-05",             // short, truncated before the time head
		"2026-10-05T01:00:00",    // explicit zone missing
		"2026-13-05T01:00:00Z",   // illegal month
		"2026-10-05T01:00:00.xZ", // non-numeric fractional second
		"2026-10-05T01:00:00Z ",  // trailing text after the zone
	}
	for _, badText := range cases {
		t.Run(badText, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "service.db")
			st := openStoreAt(t, path)
			router := NewRouter(st)
			posted := seedTwoEvents(t, router)
			corruptEventTime(t, path, 2, posted[0]["hash"].(string), badText)

			// limit=1 already read the valid seq 1 to size the page; reaching
			// the corrupt seq 2 must fail the whole request as storage
			// unavailable, never return that first page or a cursor.
			recorder := getEventsQuery(t, router, "to=2026-10-06T00:00:00Z&limit=1")
			wantStorageError(t, recorder, "events", "next_after_seq")

			// Parameter validation still beats the storage failure.
			wantInvalidQuery(t, router, "to=2026-10-06T00:00:00Z&limit=0")
			wantInvalidQuery(t, router, "from=notatime&to=2026-10-06T00:00:00Z")

			// A cursor past the bad row never scans it and pages normally.
			events, next := decodePage(t, getEventsQuery(t, router, "to=2026-10-06T00:00:00Z&after_seq=2"))
			if len(events) != 0 || next != nil {
				t.Fatalf("after_seq=2: events = %v next = %v, want empty page and null cursor", events, next)
			}
			// An account filter that excludes the bad row pages normally.
			events, next = decodePage(t, getEventsQuery(t, router, "account=bob&to=2026-10-06T00:00:00Z"))
			if len(events) != 0 || next != nil {
				t.Fatalf("account=bob: events = %v next = %v, want empty page and null cursor", events, next)
			}

			// No time window: the corrupt text is read back verbatim and is
			// neither normalized nor rejected.
			events, _ = decodePage(t, getEventsQuery(t, router, ""))
			wantSeqs(t, events, 1, 2)
			if events[1]["occurred_at"] != badText {
				t.Fatalf("windowless read normalized corrupt text to %v, want %q", events[1]["occurred_at"], badText)
			}

			// The chain still verifies: the stored hash matches the bad text
			// and its back-link, and timestamp spelling is not a chain rule.
			verify := getLedgerVerifyRequest(t, router, "/ledger/verify")
			if body := decodeVerify(t, verify); !body.Valid || body.Checked != 2 || body.FirstInvalidSeq != nil {
				t.Fatalf("verify = %+v, want a valid chain of 2 despite the corrupt timestamp text", body)
			}
			closeStore(t, st)
		})
	}
}

func TestGetEventsRecoversAfterCorruptOccurredAt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st := openStoreAt(t, path)
	router := NewRouter(st)
	posted := seedTwoEvents(t, router)

	// Only the second record's time is truncated; every other field stays.
	corruptEventTime(t, path, 2, posted[0]["hash"].(string), "2026-10-05")

	// The exact reported scenario: two legal records, the second one's time
	// shortened, and to/limit=1 must surface the storage error instead of a
	// successful first page.
	wantStorageError(t, getEventsQuery(t, router, "to=2026-10-06T00:00:00Z&limit=1"), "events", "next_after_seq")

	// The ledger was not rewritten by the failed reads: it still verifies.
	verify := getLedgerVerifyRequest(t, router, "/ledger/verify")
	if body := decodeVerify(t, verify); !body.Valid || body.Checked != 2 || body.FirstInvalidSeq != nil {
		t.Fatalf("verify after failed reads = %+v, want valid/2/null", body)
	}

	// Restore the original timestamp text and its stored hash.
	corrupt(t, path, "UPDATE events SET occurred_at = ?, hash = ? WHERE seq = 2",
		"2026-10-05T01:00:00Z", posted[1]["hash"])

	// The original windowed pagination and cursor come back unchanged.
	events, next := decodePage(t, getEventsQuery(t, router, "to=2026-10-06T00:00:00Z&limit=1"))
	wantSeqs(t, events, 1)
	if next != float64(1) {
		t.Fatalf("first page next_after_seq = %v, want 1", next)
	}
	events, next = decodePage(t, getEventsQuery(t, router, "to=2026-10-06T00:00:00Z&limit=1&after_seq=1"))
	wantSeqs(t, events, 2)
	if next != nil {
		t.Fatalf("second page next_after_seq = %v, want null", next)
	}
	if events[0]["occurred_at"] != "2026-10-05T01:00:00Z" || events[0]["hash"] != posted[1]["hash"] {
		t.Fatalf("restored second record = %v, want the original fields and hash", events[0])
	}

	verify = getLedgerVerifyRequest(t, router, "/ledger/verify")
	if body := decodeVerify(t, verify); !body.Valid || body.Checked != 2 || body.FirstInvalidSeq != nil {
		t.Fatalf("verify after recovery = %+v, want valid/2/null", body)
	}
	closeStore(t, st)
}

package api

// Regression tests for windowed GET /events queries over a ledger whose
// stored occurred_at text no longer matches the append-produced UTC shape
// (empty, truncated, missing the zone, an impossible date or a non-numeric
// fraction):
//
//   - a windowed query that reads such a record answers 503
//     storage_unavailable with a bare top-level error object — no partial
//     page, no cursor — even when valid records were scanned first;
//   - a record the account filter or after_seq already excludes never
//     reaches the instant comparison, so the rest of the ledger queries
//     normally;
//   - invalid query parameters still win a 400 invalid_audit_input over the
//     storage anomaly;
//   - a query without a time window keeps returning the stored text
//     verbatim, neither renormalizing nor rejecting it;
//   - success or failure, the query never mutates the ledger: verification
//     flags the tampered record while it is tampered, and restoring the
//     original occurred_at restores the original pagination.

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// corruptOccurredAt rewrites one stored record's occurred_at through an
// independent connection, leaving its hash and every other field untouched.
func corruptOccurredAt(t *testing.T, path string, seq int64, occurredAt string) {
	t.Helper()
	corrupt(t, path, "UPDATE events SET occurred_at = ? WHERE seq = ?", occurredAt, seq)
}

// seedTwoEvents appends the two legal records most of these tests start
// from and returns their 201 bodies.
func seedTwoEvents(t *testing.T, router http.Handler) []map[string]any {
	t.Helper()
	return []map[string]any{
		mustPost(t, router, eventBody("alice", "2026-10-05T00:00:00Z")),
		mustPost(t, router, eventBody("bob", "2026-10-05T12:00:00Z")),
	}
}

// TestGetEventsWindowCorruptOccurredAtSpecExample is the reported case: two
// legal records, the second one's occurred_at shortened to a bare date, then
// a windowed first-page query. The read of the truncated text must fail the
// whole request instead of letting the first page succeed.
func TestGetEventsWindowCorruptOccurredAtSpecExample(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st := openStoreAt(t, path)
	defer closeStore(t, st)
	router := NewRouter(st)
	seedTwoEvents(t, router)

	corruptOccurredAt(t, path, 2, "2026-10-05")

	recorder := getEventsQuery(t, router, "to=2026-10-06T00:00:00Z&limit=1")
	wantStorageError(t, recorder, "events", "next_after_seq")
}

// TestGetEventsWindowCorruptOccurredAtReturns503 covers the other broken
// shapes a stored occurred_at can take; each is a storage anomaly, never a
// record to skip or compare leniently.
func TestGetEventsWindowCorruptOccurredAtReturns503(t *testing.T) {
	cases := []struct {
		name       string
		occurredAt string
		rawQuery   string
	}{
		{"empty text", "", "to=2027-01-01T00:00:00Z"},
		{"truncated to a bare date", "2026-10-05", "to=2027-01-01T00:00:00Z"},
		{"truncated mid-time", "2026-10-05T12:0", "to=2027-01-01T00:00:00Z"},
		{"missing zone", "2026-10-05T12:00:00", "to=2027-01-01T00:00:00Z"},
		{"fraction without zone", "2026-10-05T12:00:00.5", "to=2027-01-01T00:00:00Z"},
		{"non-numeric fraction", "2026-10-05T12:00:00.abcZ", "to=2027-01-01T00:00:00Z"},
		{"empty fraction", "2026-10-05T12:00:00.Z", "to=2027-01-01T00:00:00Z"},
		{"impossible date", "2026-13-40T12:00:00Z", "to=2027-01-01T00:00:00Z"},
		{"impossible time", "2026-10-05T25:61:00Z", "to=2027-01-01T00:00:00Z"},
		{"leap second", "2026-10-05T12:00:60Z", "to=2027-01-01T00:00:00Z"},
		{"offset instead of Z", "2026-10-05T12:00:00+08:00", "to=2027-01-01T00:00:00Z"},
		{"short text under a from bound", "2026-10-05", "from=2026-01-01T00:00:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "service.db")
			st := openStoreAt(t, path)
			defer closeStore(t, st)
			router := NewRouter(st)
			seedTwoEvents(t, router)

			corruptOccurredAt(t, path, 2, tc.occurredAt)

			recorder := getEventsQuery(t, router, tc.rawQuery+"&limit=2")
			wantStorageError(t, recorder, "events", "next_after_seq")
		})
	}
}

// TestGetEventsWindowCorruptRecordFailsAfterValidPrefix seeds three records
// and corrupts the last one: the scan reads valid records before hitting the
// anomaly, yet the request must still fail as a whole rather than return the
// valid prefix.
func TestGetEventsWindowCorruptRecordFailsAfterValidPrefix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st := openStoreAt(t, path)
	defer closeStore(t, st)
	router := NewRouter(st)
	seedTwoEvents(t, router)
	mustPost(t, router, eventBody("carol", "2026-10-05T18:00:00Z"))

	corruptOccurredAt(t, path, 3, "2026-10-05T18:00:00")

	// The window and the limit cover all three records, so the two valid
	// ones are read before the scan reaches the broken occurred_at.
	recorder := getEventsQuery(t, router,
		"from=2026-10-05T00:00:00Z&to=2026-10-06T00:00:00Z&limit=10")
	wantStorageError(t, recorder, "events", "next_after_seq")
}

// TestGetEventsWindowFiltersExcludeCorruptRecord confirms the anomaly only
// fires for records the query actually reads: an account filter or an
// after_seq cursor that already excludes the broken record leaves the rest
// of the ledger queryable.
func TestGetEventsWindowFiltersExcludeCorruptRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st := openStoreAt(t, path)
	defer closeStore(t, st)
	router := NewRouter(st)
	seedTwoEvents(t, router)

	corruptOccurredAt(t, path, 1, "2026-10-05")

	// The account filter excludes the broken record.
	events, next := decodePage(t, getEventsQuery(t, router,
		"account=bob&from=2026-10-05T00:00:00Z&to=2026-10-06T00:00:00Z"))
	wantSeqs(t, events, 2)
	if next != nil {
		t.Fatalf("next_after_seq = %v, want null", next)
	}

	// A cursor past the broken record excludes it too.
	events, next = decodePage(t, getEventsQuery(t, router,
		"after_seq=1&from=2026-10-05T00:00:00Z&to=2026-10-06T00:00:00Z"))
	wantSeqs(t, events, 2)
	if next != nil {
		t.Fatalf("next_after_seq = %v, want null", next)
	}

	// Reading the broken record itself still fails the request.
	recorder := getEventsQuery(t, router,
		"account=alice&from=2026-10-05T00:00:00Z&to=2026-10-06T00:00:00Z")
	wantStorageError(t, recorder, "events", "next_after_seq")
}

// TestGetEventsCorruptOccurredAtParamErrorWins checks that malformed query
// parameters are rejected with 400 invalid_audit_input even while the ledger
// holds a broken occurred_at.
func TestGetEventsCorruptOccurredAtParamErrorWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st := openStoreAt(t, path)
	defer closeStore(t, st)
	router := NewRouter(st)
	seedTwoEvents(t, router)

	corruptOccurredAt(t, path, 2, "2026-10-05")

	for _, query := range []string{
		"limit=0&to=2026-10-06T00:00:00Z",
		"from=notatime",
		"from=2026-10-06T00:00:00Z&to=2026-10-05T00:00:00Z",
		"after_seq=-1&to=2026-10-06T00:00:00Z",
	} {
		wantInvalidQuery(t, router, query)
	}
}

// TestGetEventsWithoutWindowReturnsStoredCorruptValue confirms a query
// without a time window never renormalizes or rejects the stored text: the
// broken occurred_at comes back verbatim.
func TestGetEventsWithoutWindowReturnsStoredCorruptValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st := openStoreAt(t, path)
	defer closeStore(t, st)
	router := NewRouter(st)
	seedTwoEvents(t, router)

	corruptOccurredAt(t, path, 2, "2026-10-05")

	events, next := decodePage(t, getEventsQuery(t, router, ""))
	wantSeqs(t, events, 1, 2)
	if next != nil {
		t.Fatalf("next_after_seq = %v, want null", next)
	}
	if events[1]["occurred_at"] != "2026-10-05" {
		t.Fatalf("occurred_at = %v, want the stored text verbatim", events[1]["occurred_at"])
	}

	// An account filter alone is not a time window either.
	events, _ = decodePage(t, getEventsQuery(t, router, "account=bob"))
	wantSeqs(t, events, 2)
	if events[0]["occurred_at"] != "2026-10-05" {
		t.Fatalf("occurred_at = %v, want the stored text verbatim", events[0]["occurred_at"])
	}
}

// TestGetEventsCorruptOccurredAtLedgerUnchanged runs the failing and the
// passing queries between verification passes: the queries must not mutate
// the ledger, verification must flag the tampered record while it is
// tampered, and restoring the original occurred_at must restore the original
// pagination and let appends chain onto the original tail.
func TestGetEventsCorruptOccurredAtLedgerUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st := openStoreAt(t, path)
	defer closeStore(t, st)
	router := NewRouter(st)
	posted := seedTwoEvents(t, router)

	verify := func() map[string]any {
		t.Helper()
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/ledger/verify", nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("verify: status = %d (%s)", recorder.Code, recorder.Body.String())
		}
		return decodeEvent(t, recorder)
	}

	// Baseline: the windowed page and the chain are intact.
	const windowQuery = "from=2026-10-05T00:00:00Z&to=2026-10-06T00:00:00Z"
	events, _ := decodePage(t, getEventsQuery(t, router, windowQuery))
	wantSeqs(t, events, 1, 2)
	if result := verify(); result["valid"] != true || result["checked"] != float64(2) {
		t.Fatalf("baseline verify = %v, want a valid chain of 2 records", result)
	}

	corruptOccurredAt(t, path, 2, "2026-10-05")

	// The windowed query fails; the unwindowed one succeeds. Neither may
	// touch the ledger.
	wantStorageError(t, getEventsQuery(t, router, windowQuery), "events", "next_after_seq")
	if recorder := getEventsQuery(t, router, ""); recorder.Code != http.StatusOK {
		t.Fatalf("unwindowed query: status = %d", recorder.Code)
	}

	// The tampered occurred_at breaks the stored hash at seq 2.
	if result := verify(); result["valid"] != false ||
		result["checked"] != float64(2) || result["first_invalid_seq"] != float64(2) {
		t.Fatalf("verify after tampering = %v, want invalid/2/first 2", result)
	}

	// Restoring the original text restores the original pagination, and no
	// sequence number was consumed by the failed or successful queries.
	corruptOccurredAt(t, path, 2, posted[1]["occurred_at"].(string))
	events, next := decodePage(t, getEventsQuery(t, router, windowQuery))
	wantSeqs(t, events, 1, 2)
	if next != nil {
		t.Fatalf("next_after_seq = %v, want null", next)
	}
	wantEventsMatchPosted(t, events, posted)
	if result := verify(); result["valid"] != true || result["checked"] != float64(2) {
		t.Fatalf("restored verify = %v, want a valid chain of 2 records", result)
	}

	third := mustPost(t, router, eventBody("carol", "2026-10-05T18:00:00Z"))
	if third["seq"] != float64(3) || third["prev_hash"] != posted[1]["hash"] {
		t.Fatalf("third = %v, want seq 3 chained to %s", third, posted[1]["hash"])
	}
}

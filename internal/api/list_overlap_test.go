package api

// Regression tests for the single-committed-view semantics of one
// GET /events page while an append lands mid-scan:
//
//   - the in-flight page and its next_after_seq cursor come from one
//     committed snapshot: a matching record committed after the query
//     obtained its first matching record neither joins the page nor opens
//     a cursor, and a non-matching one leaves page and cursor untouched;
//   - once the read finishes, later requests see the new record: the
//     re-read first page repeats with the cursor open at seq 3 and the
//     next page carries exactly the appended record (matching case), or
//     the re-read page keeps its null cursor because the more-records
//     check only counts matching records (non-matching case);
//   - both scenarios end with the ledger verifying valid/4/null, so the
//     overlapping reads never rewrote a record or a chain value.
//
// The overlap is controlled, not guessed by timing or by firing requests
// concurrently: the events table is swapped for a view whose hash column
// passes through a test-only scalar function (the same view-swap idiom as
// failReadsAt), and that function parks the GET handler's row scan inside
// its read transaction at the moment the second matching row is
// materialized — after the first matching record was already delivered to
// the service. The test then commits the append through POST /events (an
// INSTEAD OF trigger keeps the write landing in the real table) and only
// afterwards releases the scan. If the rendezvous never happens the test
// fails on a timeout instead of skipping the scenario.

import (
	"database/sql/driver"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sqlite "modernc.org/sqlite"
)

// overlapPageQuery asks for the alice records inside the seed window, two
// per page.
const overlapPageQuery = "account=alice&from=2026-10-05T00:00:00Z&to=2026-10-06T00:00:00Z&limit=2"

// overlapGateName is the SQL name of the test-only scalar function the
// gating view wraps around the hash column.
const overlapGateName = "test_list_overlap_gate"

func init() {
	// Registered once for the whole test binary, before any store
	// connection opens; every connection created afterwards sees it.
	sqlite.MustRegisterScalarFunction(overlapGateName, 2, overlapGateFunc)
}

// listOverlapGate parks one in-flight list scan. entered is closed when
// the scan materializes the row with parkSeq; release lets it finish.
type listOverlapGate struct {
	parkSeq     int64
	entered     chan struct{}
	release     chan struct{}
	parked      atomic.Bool
	releaseOnce sync.Once
}

// releaseScan unblocks a parked scan; safe to call more than once.
func (g *listOverlapGate) releaseScan() {
	g.releaseOnce.Do(func() { close(g.release) })
}

// activeOverlapGate is the gate the running overlap test armed; it is nil
// outside the overlap window so every other query passes through.
var activeOverlapGate atomic.Pointer[listOverlapGate]

// overlapGateFunc implements the test-only scalar function: it returns the
// hash it is given unchanged, but the first call for the armed gate's
// parkSeq parks the calling query until the test releases it.
func overlapGateFunc(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	if gate := activeOverlapGate.Load(); gate != nil {
		if seq, ok := args[0].(int64); ok && seq == gate.parkSeq && gate.parked.CompareAndSwap(false, true) {
			close(gate.entered)
			<-gate.release
		}
	}
	return args[1], nil
}

// gateListScan swaps the events table for a view that routes the hash
// column through the gate function, plus a trigger that keeps appends
// writing the real table while the view stands in.
func gateListScan(t *testing.T, path string) {
	t.Helper()
	corrupt(t, path, "ALTER TABLE events RENAME TO events_data")
	corrupt(t, path, "CREATE VIEW events AS SELECT seq, account, operation, resource, result, occurred_at, prev_hash, "+
		overlapGateName+"(seq, hash) AS hash FROM events_data")
	corrupt(t, path, `CREATE TRIGGER events_overlap_insert INSTEAD OF INSERT ON events BEGIN
		INSERT INTO events_data (seq, account, operation, resource, result, occurred_at, prev_hash, hash)
		VALUES (NEW.seq, NEW.account, NEW.operation, NEW.resource, NEW.result, NEW.occurred_at, NEW.prev_hash, NEW.hash);
	END`)
}

// ungateListScan puts the real events table back.
func ungateListScan(t *testing.T, path string) {
	t.Helper()
	corrupt(t, path, "DROP TRIGGER events_overlap_insert")
	corrupt(t, path, "DROP VIEW events")
	corrupt(t, path, "ALTER TABLE events_data RENAME TO events")
}

// waitOverlap fails unless ch closes promptly: the controlled overlap is a
// hard requirement of these tests, never a skippable nice-to-have.
func waitOverlap(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s: the controlled overlap was not established", what)
	}
}

// overlapSeedEvents are the three seed records — alice, bob, alice on
// seq 1-3 — every occurred_at inside the query window.
func overlapSeedEvents() []eventRequest {
	return []eventRequest{
		{Account: "alice", Operation: "login", Resource: "console", Result: "ok", OccurredAt: "2026-10-05T01:00:00.2500Z"},
		{Account: "bob", Operation: "logout", Resource: "console", Result: "ok", OccurredAt: "2026-10-05T02:00:00Z"},
		{Account: "alice", Operation: "export", Resource: "report", Result: "denied", OccurredAt: "2026-10-05T11:00:00+08:00"},
	}
}

// runMidScanAppend seeds the ledger, parks the first-page query after its
// first matching record, commits midEvent through POST /events while the
// read transaction is held open, then releases the scan. It returns the
// router, the finished page and every append response in seq order. Any
// failure of the controlled overlap fails the test outright.
func runMidScanAppend(t *testing.T, mid eventRequest) (http.Handler, []map[string]any, any, []map[string]any) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "service.db")
	st := openStoreAt(t, path)
	t.Cleanup(func() { closeStore(t, st) })
	router := NewRouter(st)

	posted := make([]map[string]any, 0, 4)
	for i, seed := range overlapSeedEvents() {
		event := mustPost(t, router, mustEventJSON(t, seed))
		if event["seq"] != float64(i+1) {
			t.Fatalf("seed %d seq = %v, want %d", i, event["seq"], i+1)
		}
		posted = append(posted, event)
	}

	gate := &listOverlapGate{
		parkSeq: 3,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	t.Cleanup(gate.releaseScan) // never leave a parked scan behind on failure
	gateListScan(t, path)
	activeOverlapGate.Store(gate)
	defer activeOverlapGate.Store(nil)

	pageDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		pageDone <- getEventsQuery(t, router, overlapPageQuery)
	}()

	// The query must reach the rendezvous: its read transaction has already
	// delivered the first matching record and is parked materializing the
	// second one.
	waitOverlap(t, gate.entered, "the list query to park mid-scan")

	// The append commits while the read transaction is held open.
	midPosted := mustPost(t, router, mustEventJSON(t, mid))
	if midPosted["seq"] != float64(4) {
		t.Fatalf("mid-scan append seq = %v, want 4", midPosted["seq"])
	}
	posted = append(posted, midPosted)

	// The parked query cannot have finished before the append committed.
	select {
	case <-pageDone:
		t.Fatalf("the list query finished before the mid-scan append was released")
	default:
	}

	gate.releaseScan()

	var recorder *httptest.ResponseRecorder
	select {
	case recorder = <-pageDone:
	case <-time.After(10 * time.Second):
		t.Fatalf("the parked list query did not finish after the gate was released")
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	events, next := decodePage(t, recorder)

	ungateListScan(t, path)
	return router, events, next, posted
}

// wantEventsMatchPosts fails unless events carries exactly the eight
// published fields of each matching append response, in order.
func wantEventsMatchPosts(t *testing.T, events []map[string]any, posted ...map[string]any) {
	t.Helper()
	wantEventsMatchPosted(t, events, posted)
	for i, event := range events {
		if len(event) != 8 {
			t.Fatalf("event %d keys = %v, want the 8 append response fields", i, event)
		}
	}
}

// wantValidChainOf4 asserts the post-scenario verify contract: the four
// appended records still form one valid, untouched chain.
func wantValidChainOf4(t *testing.T, router http.Handler) {
	t.Helper()
	recorder := getLedgerVerifyRequest(t, router, "/ledger/verify")
	if recorder.Code != http.StatusOK {
		t.Fatalf("verify: status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if body := decodeVerify(t, recorder); !body.Valid || body.Checked != 4 || body.FirstInvalidSeq != nil {
		t.Fatalf("verify = %+v, want valid/4/null", body)
	}
	assertVerifyExactFields(t, recorder)
}

func TestGetEventsPageSnapshotExcludesMidScanMatchingAppend(t *testing.T) {
	mid := eventRequest{Account: "alice", Operation: "import", Resource: "billing", Result: "ok", OccurredAt: "2026-10-05T04:00:00.5Z"}
	router, events, next, posted := runMidScanAppend(t, mid)

	// The in-flight page reflects one committed view: seq 1 and 3 with a
	// null cursor; the record committed mid-scan neither joined the page
	// nor opened a cursor claiming more matches.
	wantSeqs(t, events, 1, 3)
	if next != nil {
		t.Fatalf("next_after_seq = %v, want null", next)
	}
	wantEventsMatchPosts(t, events, posted[0], posted[2])

	// After the read, the same first page now sees the extra match: the
	// cursor opens at seq 3.
	events, next = decodePage(t, getEventsQuery(t, router, overlapPageQuery))
	wantSeqs(t, events, 1, 3)
	if next != float64(3) {
		t.Fatalf("next_after_seq = %v, want 3", next)
	}
	wantEventsMatchPosts(t, events, posted[0], posted[2])

	// The cursor leads to exactly the appended record and closes without
	// repeating the old records.
	events, next = decodePage(t, getEventsQuery(t, router, overlapPageQuery+"&after_seq=3"))
	wantSeqs(t, events, 4)
	if next != nil {
		t.Fatalf("next_after_seq = %v, want null", next)
	}
	wantEventsMatchPosts(t, events, posted[3])

	wantValidChainOf4(t, router)
}

func TestGetEventsPageSnapshotExcludesMidScanNonMatchingAppend(t *testing.T) {
	mid := eventRequest{Account: "bob", Operation: "import", Resource: "billing", Result: "ok", OccurredAt: "2026-10-05T04:00:00.5Z"}
	router, events, next, posted := runMidScanAppend(t, mid)

	// The non-matching append leaves the in-flight page and its cursor
	// untouched.
	wantSeqs(t, events, 1, 3)
	if next != nil {
		t.Fatalf("next_after_seq = %v, want null", next)
	}
	wantEventsMatchPosts(t, events, posted[0], posted[2])

	// The re-read page is unchanged too: the more-records check only
	// counts records matching the query, so the bob record opens no cursor.
	events, next = decodePage(t, getEventsQuery(t, router, overlapPageQuery))
	wantSeqs(t, events, 1, 3)
	if next != nil {
		t.Fatalf("next_after_seq = %v, want null", next)
	}
	wantEventsMatchPosts(t, events, posted[0], posted[2])

	// Past seq 3 there is still nothing for alice.
	events, next = decodePage(t, getEventsQuery(t, router, overlapPageQuery+"&after_seq=3"))
	if len(events) != 0 || next != nil {
		t.Fatalf("events = %v next = %v, want empty page and null cursor", events, next)
	}

	wantValidChainOf4(t, router)
}

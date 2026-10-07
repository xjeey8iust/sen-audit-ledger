package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sqlite "modernc.org/sqlite"
)

// This file pins the single-committed-view semantics of GET /events against
// an append that commits while a page read is still in flight — not between
// two requests, but after the read has fetched its first matching record and
// before it completes.
//
// The overlap is controlled, not guessed. modernc.org/sqlite lets a test
// binary register a collation named BINARY, which SQLite then uses for the
// "account = ?" comparison in the ListEvents SELECT (the events table has no
// index on account, so the comparison runs once per scanned row, in ascending
// seq order). The override is a faithful bytewise comparison — strings.Compare
// is exactly SQLite's BINARY semantics — so with no probe armed it is
// transparent to every query in this test binary. While a probe is armed it
// rendezvous with the test on every comparison: the read parks inside the
// database scan, the append commits through the real POST /events route on
// another connection (WAL readers never block the writer), and the read then
// finishes. Every step is a channel rendezvous with a timeout that turns a
// broken overlap into an explicit test failure; nothing relies on issuing
// requests concurrently or on delays.
//
// The override lives only in this test binary: it adds no production
// interface, query parameter or runtime configuration, and no production
// source changes.

// overlapTimeout bounds every wait in the controlled overlap. It never
// substitutes for synchronization — each step is a channel rendezvous — it
// only turns a broken overlap into an explicit failure instead of a hang.
const overlapTimeout = 10 * time.Second

// overlapComparison is one account comparison the ListEvents scan performs,
// observed through the test-only collation: stored is the account text of the
// record under evaluation, wanted is the account the query filters by.
type overlapComparison struct {
	stored string
	wanted string
}

// listOverlapProbe parks the ListEvents scan at chosen rows so the test can
// commit an append while the read is in flight. The scan sends one
// overlapComparison per record it evaluates and blocks until the test
// releases it, so the interleaving is exact and repeatable.
type listOverlapProbe struct {
	compared  chan overlapComparison // scan -> test, one per evaluated record
	resume    chan struct{}          // test -> scan, releases one parked evaluation
	abort     chan struct{}          // closed to release everything on failure
	abortOnce sync.Once
}

// armedOverlapProbe is the probe the test-only collation reports to, or nil
// outside a controlled overlap, where the collation is a pure passthrough.
var armedOverlapProbe atomic.Pointer[listOverlapProbe]

var overlapCollationOnce sync.Once
var overlapCollationErr error

// armOverlapCollation installs the process-wide BINARY collation override
// exactly once for this test binary. Registered before any store is opened,
// it applies to every connection the stores open afterwards.
func armOverlapCollation(t *testing.T) {
	t.Helper()
	overlapCollationOnce.Do(func() {
		overlapCollationErr = sqlite.RegisterCollationUtf8("BINARY", func(left, right string) int {
			if probe := armedOverlapProbe.Load(); probe != nil {
				probe.serve(left, right)
			}
			return strings.Compare(left, right)
		})
	})
	if overlapCollationErr != nil {
		t.Fatalf("register test collation: %v", overlapCollationErr)
	}
}

// serve reports one comparison to the test and parks until released, unless
// the probe has been aborted.
func (p *listOverlapProbe) serve(stored, wanted string) {
	select {
	case p.compared <- overlapComparison{stored, wanted}:
	case <-p.abort:
		return
	}
	select {
	case <-p.resume:
	case <-p.abort:
	}
}

// stop releases every parked and future evaluation of this probe.
func (p *listOverlapProbe) stop() {
	p.abortOnce.Do(func() { close(p.abort) })
}

// nextComparison waits for the scan to evaluate the next record and requires
// it to be the expected one. A timeout or a mismatch means the controlled
// overlap did not establish and fails the test outright.
func (p *listOverlapProbe) nextComparison(t *testing.T, want overlapComparison) {
	t.Helper()
	select {
	case got := <-p.compared:
		if got != want {
			t.Fatalf("controlled overlap: scan evaluated (%q, %q), want (%q, %q)",
				got.stored, got.wanted, want.stored, want.wanted)
		}
	case <-time.After(overlapTimeout):
		t.Fatalf("controlled overlap: no record evaluated within %s; the read did not reach the expected point", overlapTimeout)
	}
}

// release lets the parked evaluation proceed.
func (p *listOverlapProbe) release(t *testing.T) {
	t.Helper()
	select {
	case p.resume <- struct{}{}:
	case <-time.After(overlapTimeout):
		t.Fatalf("controlled overlap: the scan did not take the release within %s", overlapTimeout)
	}
}

// wantPostedEvent requires one page record to carry exactly the eight append
// response fields, each equal to what POST /events returned for that record:
// the business fields, the occurred_at text as appended, and both chain
// values. Counting records is never enough.
func wantPostedEvent(t *testing.T, event, posted map[string]any) {
	t.Helper()
	if len(event) != 8 {
		t.Fatalf("event keys = %v, want the 8 append response fields", event)
	}
	for _, key := range []string{"account", "operation", "resource", "result", "occurred_at", "seq", "prev_hash", "hash"} {
		if event[key] != posted[key] {
			t.Fatalf("event %s = %v, want the appended %v", key, event[key], posted[key])
		}
	}
}

// runListOverlapScenario drives one controlled overlap. The ledger starts as
// alice, bob, alice at seq 1, 2, 3, all inside one query window. A GET
// /events read filtered to alice over that window with limit 2 is parked
// after fetching its first matching record (seq 1) and before completing;
// one more in-window record for overlapAccount is appended through POST
// /events and commits while the read is parked; the read then finishes. The
// original page must reflect the pre-append snapshot exactly, and later
// requests must see the appended record according to the filter.
func runListOverlapScenario(t *testing.T, overlapAccount string) {
	t.Helper()
	armOverlapCollation(t)
	router := NewRouter(openTestStore(t))

	// Seed the ledger through the public append route and keep every
	// response: expected page records are these exact bodies.
	posted := make([]map[string]any, 0, 3)
	for i, seed := range []struct{ account, at string }{
		{"alice", "2026-10-05T00:00:00Z"},
		{"bob", "2026-10-05T01:00:00Z"},
		{"alice", "2026-10-05T02:00:00Z"},
	} {
		posted = append(posted, mustPost(t, router, eventBody(seed.account, seed.at)))
		if posted[i]["seq"] != float64(i+1) {
			t.Fatalf("seed append seq = %v, want %d", posted[i]["seq"], i+1)
		}
	}

	probe := &listOverlapProbe{
		compared: make(chan overlapComparison),
		resume:   make(chan struct{}),
		abort:    make(chan struct{}),
	}
	armedOverlapProbe.Store(probe)
	t.Cleanup(func() {
		armedOverlapProbe.Store(nil)
		probe.stop()
	})

	// Account filter, time window and page size all apply at once.
	const pageQuery = "account=alice" +
		"&from=2026-10-05T00:00:00Z&to=2026-10-06T00:00:00Z&limit=2"

	// Start the page read on its own goroutine; the probe parks it mid-scan.
	getDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { getDone <- getEventsQuery(t, router, pageQuery) }()

	// The scan evaluates seq 1 (alice = alice): the read has started and is
	// fetching its first matching record.
	probe.nextComparison(t, overlapComparison{"alice", "alice"})
	probe.release(t)
	// The scan reaches seq 2 (bob): seq 1 is fully fetched into the page and
	// the query is still running. This is the overlap point.
	probe.nextComparison(t, overlapComparison{"bob", "alice"})

	// Append one more in-window record while the read is parked. The POST
	// runs on its own goroutine so the test keeps owning the rendezvous even
	// if the append path ever blocked.
	postDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		postDone <- postEventBody(t, router, eventBody(overlapAccount, "2026-10-05T03:00:00Z"))
	}()
	var postRecorder *httptest.ResponseRecorder
	select {
	case postRecorder = <-postDone:
	case <-time.After(overlapTimeout):
		t.Fatalf("controlled overlap: the append did not commit within %s while the read was parked", overlapTimeout)
	}
	if postRecorder.Code != http.StatusCreated {
		t.Fatalf("overlap append: status = %d (%s)", postRecorder.Code, postRecorder.Body.String())
	}
	appended := decodeEvent(t, postRecorder)
	if appended["seq"] != float64(4) {
		t.Fatalf("overlap append seq = %v, want 4", appended["seq"])
	}

	// The append committed before the original read completed: the read is
	// still parked inside the scan, so it cannot have finished.
	select {
	case <-getDone:
		t.Fatal("the page read completed before the overlapping append committed")
	default:
	}

	// Let the read finish. Its snapshot holds seq 1, 2, 3 only, so exactly
	// one more record is evaluated (seq 3, alice) before the scan runs out.
	// Any further comparison means the read observed the record appended
	// after its snapshot — the overlap failed and the test must not accept
	// whatever the page happens to contain.
	probe.release(t)
	probe.nextComparison(t, overlapComparison{"alice", "alice"})
	probe.release(t)
	var pageRecorder *httptest.ResponseRecorder
	select {
	case pageRecorder = <-getDone:
	case extra := <-probe.compared:
		t.Fatalf("the read evaluated an extra record (%q, %q) past its pre-append snapshot", extra.stored, extra.wanted)
	case <-time.After(overlapTimeout):
		t.Fatalf("controlled overlap: the page read did not finish within %s", overlapTimeout)
	}
	armedOverlapProbe.Store(nil)

	// The original page is the pre-append snapshot: seq 1 and 3, full field
	// equality with the append responses, and no cursor — the record
	// appended mid-read must not reach this page nor open a cursor on it.
	if pageRecorder.Code != http.StatusOK {
		t.Fatalf("page read: status = %d (%s)", pageRecorder.Code, pageRecorder.Body.String())
	}
	events, next := decodePage(t, pageRecorder)
	wantSeqs(t, events, 1, 3)
	if next != nil {
		t.Fatalf("next_after_seq = %v, want null: the mid-read append must not leak into this page", next)
	}
	wantPostedEvent(t, events[0], posted[0])
	wantPostedEvent(t, events[1], posted[2])

	// After the read finishes, the committed record is visible to later
	// requests. Re-reading the same first page still returns seq 1 and 3.
	rereadRecorder := getEventsQuery(t, router, pageQuery)
	if rereadRecorder.Code != http.StatusOK {
		t.Fatalf("reread: status = %d (%s)", rereadRecorder.Code, rereadRecorder.Body.String())
	}
	events, next = decodePage(t, rereadRecorder)
	wantSeqs(t, events, 1, 3)
	wantPostedEvent(t, events[0], posted[0])
	wantPostedEvent(t, events[1], posted[2])

	if overlapAccount == "alice" {
		// The appended record matches the filter: the first page now has a
		// successor, so the cursor points at seq 3…
		if next != float64(3) {
			t.Fatalf("reread next_after_seq = %v, want 3", next)
		}
		// …and following the cursor returns exactly the new record, with no
		// repetition of the old ones and a closed cursor.
		nextRecorder := getEventsQuery(t, router, pageQuery+"&after_seq=3")
		if nextRecorder.Code != http.StatusOK {
			t.Fatalf("next page: status = %d (%s)", nextRecorder.Code, nextRecorder.Body.String())
		}
		events, next = decodePage(t, nextRecorder)
		wantSeqs(t, events, 4)
		if next != nil {
			t.Fatalf("next page next_after_seq = %v, want null", next)
		}
		wantPostedEvent(t, events[0], appended)
	} else {
		// The appended record does not match the filter: the "more records"
		// judgement only looks at matching records, so the re-read keeps the
		// null cursor.
		if next != nil {
			t.Fatalf("reread next_after_seq = %v, want null: a non-matching append must not open a cursor", next)
		}
	}

	// Neither the reads nor the overlap rewrote anything: the whole ledger
	// still verifies as one continuous chain of four records.
	verifyRecorder := httptest.NewRecorder()
	router.ServeHTTP(verifyRecorder, httptest.NewRequest(http.MethodGet, "/ledger/verify", nil))
	if verifyRecorder.Code != http.StatusOK {
		t.Fatalf("verify: status = %d (%s)", verifyRecorder.Code, verifyRecorder.Body.String())
	}
	result := decodeEvent(t, verifyRecorder)
	if result["valid"] != true || result["checked"] != float64(4) || result["first_invalid_seq"] != nil {
		t.Fatalf("verify = %v, want valid=true checked=4 first_invalid_seq=null", result)
	}
}

// A matching record appended after a page read has fetched its first match
// but before it completes is invisible to that page: the page and its cursor
// come from one committed view. Later requests see the record.
func TestGetEventsPageStableUnderOverlappingMatchingAppend(t *testing.T) {
	runListOverlapScenario(t, "alice")
}

// A non-matching record appended at the same overlap point changes nothing
// for the in-flight page or for later reads of the same window: the
// more-records judgement only considers records that match the filter.
func TestGetEventsPageStableUnderOverlappingNonMatchingAppend(t *testing.T) {
	runListOverlapScenario(t, "bob")
}

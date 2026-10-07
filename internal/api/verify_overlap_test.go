package api

// Regression tests for the single-committed-view semantics of GET
// /ledger/verify against an append that commits while a verification pass is
// still in flight — not between two requests, but after the pass has read the
// first ledger record and before it completes:
//
//   - with an intact chain, the in-flight pass answers 200 valid with
//     checked equal to the three records its snapshot holds and
//     first_invalid_seq null: the record committed mid-scan is neither mixed
//     into this result nor mistaken for a break. A later pass verifies the
//     full chain of four, and paged reads return all four records exactly as
//     their append responses;
//   - with the stored prev_hash of seq 2 overwritten (every other field
//     kept), the in-flight pass and every later pass still answer 200 with
//     valid false, checked 2 and first_invalid_seq 2: the record committed
//     mid-scan neither skips the existing break nor extends the checked
//     count past it.
//
// The overlap is controlled, not guessed. The events table is swapped for a
// view whose hash column passes through a test-only scalar function
// (modernc.org/sqlite's RegisterScalarFunction), so every row the
// VerifyChain scan reads rendezvous with the test: the scan parks inside the
// database read, the test restores the real table and commits one more
// record through the real POST /events route on another connection (WAL
// readers never block the writer), and the scan then finishes on its
// original snapshot. Every step is a channel rendezvous with a timeout that
// turns a broken overlap into an explicit test failure; nothing relies on
// issuing requests concurrently, on fixed waits or on retries.
//
// The gate function and the view live only in this test binary: they add no
// production interface, query parameter or runtime configuration, and no
// production source changes. With no probe armed the gate returns its input
// unchanged, and the real table is restored before the append so the write
// path under test is the unmodified one.

import (
	"database/sql/driver"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sqlite "modernc.org/sqlite"
)

// verifyScanProbe parks the VerifyChain scan at chosen rows so the test can
// commit an append while the pass is in flight. The scan sends the seq of
// each row whose hash it evaluates and blocks until the test releases it, so
// the interleaving is exact and repeatable.
type verifyScanProbe struct {
	scanned   chan int64    // scan -> test, one per evaluated row
	resume    chan struct{} // test -> scan, releases one parked evaluation
	abort     chan struct{} // closed to release everything on failure
	abortOnce sync.Once
}

// armedVerifyScanProbe is the probe the test-only gate function reports to,
// or nil outside a controlled overlap, where the gate is a pure passthrough.
var armedVerifyScanProbe atomic.Pointer[verifyScanProbe]

var verifyScanGateOnce sync.Once
var verifyScanGateErr error

// armVerifyScanGate registers the process-wide gate function exactly once
// for this test binary. Registered before the store under test is opened, it
// is available to every connection the store opens afterwards.
func armVerifyScanGate(t *testing.T) {
	t.Helper()
	verifyScanGateOnce.Do(func() {
		verifyScanGateErr = sqlite.RegisterScalarFunction("audit_verify_scan_gate", 2,
			func(ctx *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
				if probe := armedVerifyScanProbe.Load(); probe != nil {
					if seq, ok := args[1].(int64); ok {
						probe.serve(seq)
					}
				}
				return args[0], nil
			})
	})
	if verifyScanGateErr != nil {
		t.Fatalf("register verify scan gate: %v", verifyScanGateErr)
	}
}

// serve reports one evaluated row to the test and parks until released,
// unless the probe has been aborted.
func (p *verifyScanProbe) serve(seq int64) {
	select {
	case p.scanned <- seq:
	case <-p.abort:
		return
	}
	select {
	case <-p.resume:
	case <-p.abort:
	}
}

// stop releases every parked and future evaluation of this probe.
func (p *verifyScanProbe) stop() {
	p.abortOnce.Do(func() { close(p.abort) })
}

// nextScan waits for the verification scan to evaluate the next row and
// requires it to be the expected seq. A timeout or a mismatch means the
// controlled overlap did not establish and fails the test outright.
func (p *verifyScanProbe) nextScan(t *testing.T, wantSeq int64) {
	t.Helper()
	select {
	case got := <-p.scanned:
		if got != wantSeq {
			t.Fatalf("controlled overlap: scan evaluated seq %d, want %d", got, wantSeq)
		}
	case <-time.After(overlapTimeout):
		t.Fatalf("controlled overlap: no row evaluated within %s; the verification did not reach the expected point", overlapTimeout)
	}
}

// release lets the parked evaluation proceed.
func (p *verifyScanProbe) release(t *testing.T) {
	t.Helper()
	select {
	case p.resume <- struct{}{}:
	case <-time.After(overlapTimeout):
		t.Fatalf("controlled overlap: the scan did not take the release within %s", overlapTimeout)
	}
}

// armVerifyScanView swaps the events table for a view that serves the same
// columns but routes every hash the scan reads through the gate function, so
// an armed probe observes — and can park — each row of the verification pass
// in seq order.
func armVerifyScanView(t *testing.T, path string) {
	t.Helper()
	corrupt(t, path, "ALTER TABLE events RENAME TO events_data")
	corrupt(t, path, "CREATE VIEW events AS SELECT seq, account, operation, resource, result, occurred_at, prev_hash, "+
		"audit_verify_scan_gate(hash, seq) AS hash FROM events_data")
}

// runVerifyOverlapScenario drives one controlled overlap. Three records are
// appended through POST /events and their full responses kept. Optionally the
// stored prev_hash of seq 2 is overwritten while its other fields stay as
// appended. A GET /ledger/verify pass is parked after it has read the first
// record and before it completes; one more record is appended through POST
// /events and commits while the pass is parked; the pass then finishes on
// its pre-append snapshot. Later passes and paged reads see the settled
// four-record ledger.
func runVerifyOverlapScenario(t *testing.T, breakChain bool) {
	t.Helper()
	armVerifyScanGate(t)

	path := filepath.Join(t.TempDir(), "service.db")
	st := openStoreAt(t, path)
	defer closeStore(t, st)
	router := NewRouter(st)

	// Seed the ledger through the public append route and keep every
	// response: expected records are these exact bodies.
	posted := seedEvents(t, router, 3)
	for i, event := range posted {
		if event["seq"] != float64(i+1) {
			t.Fatalf("seed append seq = %v, want %d", event["seq"], i+1)
		}
	}

	// Baseline: the committed ledger pages back exactly as appended.
	baseline, next := decodePage(t, getEventsQuery(t, router, "limit=3"))
	wantSeqs(t, baseline, 1, 2, 3)
	if next != nil {
		t.Fatalf("baseline next_after_seq = %v, want null on the only page", next)
	}
	wantEventsMatchPosted(t, baseline, posted)

	// In the broken scenario, seq 2 keeps its position and every appended
	// field except its stored prev_hash, which no longer chains to seq 1.
	brokenPrevHash := ""
	if breakChain {
		brokenPrevHash = strings.Repeat("0", 64)
		corrupt(t, path, "UPDATE events SET prev_hash = ? WHERE seq = 2", brokenPrevHash)
		recorder := getLedgerVerifyRequest(t, router, "/ledger/verify")
		if recorder.Code != http.StatusOK {
			t.Fatalf("broken baseline verify: status = %d (%s)", recorder.Code, recorder.Body.String())
		}
		body := decodeVerify(t, recorder)
		if body.Valid || body.Checked != 2 || body.FirstInvalidSeq == nil || *body.FirstInvalidSeq != 2 {
			t.Fatalf("broken baseline verify = %+v, want invalid/2/first 2", body)
		}
	}

	armVerifyScanView(t, path)

	probe := &verifyScanProbe{
		scanned: make(chan int64),
		resume:  make(chan struct{}),
		abort:   make(chan struct{}),
	}
	armedVerifyScanProbe.Store(probe)
	t.Cleanup(func() {
		armedVerifyScanProbe.Store(nil)
		probe.stop()
	})

	// Start the verification pass on its own goroutine; the probe parks it
	// mid-scan.
	verifyDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { verifyDone <- getLedgerVerifyRequest(t, router, "/ledger/verify") }()

	// The scan evaluates seq 1: the pass has started and is reading the first
	// record.
	probe.nextScan(t, 1)
	probe.release(t)
	// The scan reaches seq 2: the first record is fully read and checked and
	// the pass is still running. This is the overlap point.
	probe.nextScan(t, 2)

	// The service stays healthy with the pass parked mid-scan.
	wantHealthy(t, router)

	// Put the real table back so the append below runs the unmodified write
	// path; the parked pass keeps its own snapshot of the view.
	restoreReads(t, path)

	// Append one more record while the pass is parked. The POST runs on its
	// own goroutine so the test keeps owning the rendezvous even if the
	// append path ever blocked.
	postDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		postDone <- postEventBody(t, router, eventBody("audit", "2026-10-05T00:03:00Z"))
	}()
	var postRecorder *httptest.ResponseRecorder
	select {
	case postRecorder = <-postDone:
	case <-time.After(overlapTimeout):
		t.Fatalf("controlled overlap: the append did not commit within %s while the verification was parked", overlapTimeout)
	}
	if postRecorder.Code != http.StatusCreated {
		t.Fatalf("overlap append: status = %d (%s)", postRecorder.Code, postRecorder.Body.String())
	}
	appended := decodeEvent(t, postRecorder)
	if appended["seq"] != float64(4) {
		t.Fatalf("overlap append seq = %v, want 4", appended["seq"])
	}
	if appended["prev_hash"] != posted[2]["hash"] {
		t.Fatalf("overlap append prev_hash = %v, want the seq 3 hash %v", appended["prev_hash"], posted[2]["hash"])
	}

	// The append committed before the pass completed: the pass is still
	// parked inside the scan, so it cannot have finished.
	select {
	case <-verifyDone:
		t.Fatal("the verification pass completed before the overlapping append committed")
	default:
	}

	// Let the pass finish on its pre-append snapshot.
	probe.release(t)
	if !breakChain {
		// The intact chain reads seq 3 as well; the broken chain stops at
		// seq 2 and must not evaluate further rows.
		probe.nextScan(t, 3)
		probe.release(t)
	}
	armedVerifyScanProbe.Store(nil)

	var verifyRecorder *httptest.ResponseRecorder
	select {
	case verifyRecorder = <-verifyDone:
	case <-time.After(overlapTimeout):
		t.Fatalf("controlled overlap: the verification pass did not finish within %s", overlapTimeout)
	}
	if verifyRecorder.Code != http.StatusOK {
		t.Fatalf("overlap verify: status = %d (%s)", verifyRecorder.Code, verifyRecorder.Body.String())
	}
	assertVerifyExactFields(t, verifyRecorder)
	body := decodeVerify(t, verifyRecorder)
	if !breakChain {
		// The pass saw only the three committed records: the fourth is
		// neither mixed in nor reported as a break.
		if !body.Valid || body.Checked != 3 || body.FirstInvalidSeq != nil {
			t.Fatalf("overlap verify = %+v, want valid/3/null", body)
		}
	} else {
		// The pass stopped at the pre-existing break; the committed record
		// neither skipped it nor extended the count past it.
		if body.Valid || body.Checked != 2 || body.FirstInvalidSeq == nil || *body.FirstInvalidSeq != 2 {
			t.Fatalf("overlap verify = %+v, want invalid/2/first 2", body)
		}
	}

	// A later pass sees the settled ledger.
	recorder := getLedgerVerifyRequest(t, router, "/ledger/verify")
	if recorder.Code != http.StatusOK {
		t.Fatalf("settled verify: status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	assertVerifyExactFields(t, recorder)
	settled := decodeVerify(t, recorder)
	if !breakChain {
		if !settled.Valid || settled.Checked != 4 || settled.FirstInvalidSeq != nil {
			t.Fatalf("settled verify = %+v, want valid/4/null", settled)
		}
	} else {
		if settled.Valid || settled.Checked != 2 || settled.FirstInvalidSeq == nil || *settled.FirstInvalidSeq != 2 {
			t.Fatalf("settled verify = %+v, want invalid/2/first 2", settled)
		}
	}

	// Paged reads return all four records across the cursor boundary.
	first, next := decodePage(t, getEventsQuery(t, router, "limit=3"))
	wantSeqs(t, first, 1, 2, 3)
	if next != float64(3) {
		t.Fatalf("next_after_seq = %v, want 3", next)
	}
	last, next := decodePage(t, getEventsQuery(t, router, "limit=3&after_seq=3"))
	wantSeqs(t, last, 4)
	if next != nil {
		t.Fatalf("last page next_after_seq = %v, want null", next)
	}

	// The pre-existing records kept the business fields, seq and chain values
	// they were appended with, and the fourth record carries exactly its
	// append response. In the broken scenario seq 2 differs only in the
	// prev_hash this test overwrote; verification never repairs stored
	// values.
	expected := make([]map[string]any, 0, 4)
	expected = append(expected, posted...)
	expected = append(expected, appended)
	if breakChain {
		second := make(map[string]any, len(posted[1]))
		for key, value := range posted[1] {
			second[key] = value
		}
		second["prev_hash"] = brokenPrevHash
		expected[1] = second
	}
	wantEventsMatchPosted(t, append(first, last...), expected)
}

// An append that commits while a verification pass is parked mid-scan is
// invisible to that pass: the pass reports the pre-append chain, valid and
// complete, and the appended record appears in later passes and reads.
func TestVerifySnapshotStableUnderOverlappingAppend(t *testing.T) {
	runVerifyOverlapScenario(t, false)
}

// The same overlap over a broken chain: the in-flight pass and later passes
// keep reporting the original break at seq 2 — the committed record neither
// skips it nor counts past it.
func TestVerifyBrokenChainStableUnderOverlappingAppend(t *testing.T) {
	runVerifyOverlapScenario(t, true)
}

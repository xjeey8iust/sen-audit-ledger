package api

// Regression tests for a verification pass overlapping a concurrent append:
//
//   - a GET /ledger/verify that has read the first record and is still
//     running when a fourth record commits answers for the three-record
//     committed prefix it started from: valid, checked 3, first_invalid_seq
//     null — the late record is neither mixed into the result nor mistaken
//     for a broken chain;
//   - the same overlap on a ledger whose second record already carries a
//     wrong back-link still stops at seq 2: the committed append neither
//     skips the pre-existing break nor extends checked past it, and later
//     passes report the same break;
//   - afterwards paged reads return every record exactly as its append
//     response reported it (in the broken scenario, record 2 keeps its
//     business fields, seq and hash, with only the stored back-link
//     replaced).
//
// The overlap is proven, not inferred from timing: a store test seam parks
// the verification pass synchronously after it scans the first record, the
// test confirms the pass has not finished, lets the append commit, confirms
// the pass is still parked, and only then releases it. Every wait is
// bounded, and cleanup releases the pass and restores the seam even when
// the test fails.

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xjeey8iust/sen-audit-ledger/internal/store"
)

// verifyStepTimeout bounds every wait that synchronizes a test with the
// paused verification pass.
const verifyStepTimeout = 10 * time.Second

// pausedVerify is one in-flight GET /ledger/verify that parks after scanning
// the first ledger record until the test releases it.
type pausedVerify struct {
	readFirst chan struct{}                   // closed once the pass has scanned record 1
	done      chan *httptest.ResponseRecorder // receives the response once the pass finishes
	unblock   func()                          // lets the parked pass continue
	got       bool                            // the response has been consumed
}

// startPausedVerify installs the store's row-hook test seam and starts a
// verification pass that parks after scanning the first record. Cleanup
// releases the pass and restores the seam even when the test fails, so a
// parked pass never outlives the test.
func startPausedVerify(t *testing.T, router http.Handler) *pausedVerify {
	t.Helper()
	v := &pausedVerify{
		readFirst: make(chan struct{}),
		done:      make(chan *httptest.ResponseRecorder, 1),
	}
	unblock := make(chan struct{})
	var signalOnce, unblockOnce sync.Once
	restore := store.SetVerifyRowHook(func(seq int64) {
		if seq != 1 {
			return
		}
		signalOnce.Do(func() { close(v.readFirst) })
		select {
		case <-unblock:
		case <-time.After(verifyStepTimeout):
			// Backstop only: cleanup closes unblock, so this wait is
			// bounded even on a failing test.
		}
	})
	v.unblock = func() { unblockOnce.Do(func() { close(unblock) }) }
	go func() {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/ledger/verify", nil))
		v.done <- recorder
	}()
	t.Cleanup(func() {
		v.unblock()
		restore()
		if !v.got {
			select {
			case <-v.done:
			case <-time.After(verifyStepTimeout):
				t.Error("paused verification pass did not finish after release")
			}
		}
	})
	return v
}

// awaitFirstRow fails unless the pass reaches the first record in time.
func (v *pausedVerify) awaitFirstRow(t *testing.T) {
	t.Helper()
	select {
	case <-v.readFirst:
	case <-time.After(verifyStepTimeout):
		t.Fatal("verification pass did not reach the first record")
	}
}

// assertPending fails if the pass has already finished: the overlap with the
// append must be real, not assumed.
func (v *pausedVerify) assertPending(t *testing.T) {
	t.Helper()
	select {
	case <-v.done:
		v.got = true
		t.Fatal("verification pass finished before the concurrent append committed")
	default:
	}
}

// result waits for the finished pass and returns its response.
func (v *pausedVerify) result(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case recorder := <-v.done:
		v.got = true
		return recorder
	case <-time.After(verifyStepTimeout):
		t.Fatal("verification pass did not finish")
		return nil
	}
}

func TestVerifyInFlightSeesCommittedPrefixWhenAppendLands(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st := openStoreAt(t, path)
	defer closeStore(t, st)
	router := NewRouter(st)
	posted := seedEvents(t, router, 3)

	verify := startPausedVerify(t, router)
	verify.awaitFirstRow(t)
	verify.assertPending(t)

	// The service stays responsive while the pass is parked.
	wantHealthy(t, router)

	// The fourth record commits while the pass is parked after record 1.
	fourth := mustPost(t, router, eventBody("audit", "2026-10-05T00:03:00Z"))
	if fourth["seq"] != float64(4) {
		t.Fatalf("fourth seq = %v, want 4", fourth["seq"])
	}
	if fourth["prev_hash"] != posted[2]["hash"] {
		t.Fatalf("fourth prev_hash = %v, want the third record's hash %v", fourth["prev_hash"], posted[2]["hash"])
	}
	posted = append(posted, fourth)
	verify.assertPending(t)

	// The pass answers for the committed three-record prefix it started
	// from: the late record is neither mixed in nor mistaken for a break.
	verify.unblock()
	recorder := verify.result(t)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
	body := decodeVerify(t, recorder)
	if !body.Valid || body.Checked != 3 || body.FirstInvalidSeq != nil {
		t.Fatalf("in-flight verify = %+v, want valid/3/null", body)
	}
	assertVerifyExactFields(t, recorder)

	// A fresh pass sees the full four-record chain.
	recorder = getLedgerVerifyRequest(t, router, "/ledger/verify")
	if recorder.Code != http.StatusOK {
		t.Fatalf("re-verify status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
	body = decodeVerify(t, recorder)
	if !body.Valid || body.Checked != 4 || body.FirstInvalidSeq != nil {
		t.Fatalf("re-verify = %+v, want valid/4/null", body)
	}
	assertVerifyExactFields(t, recorder)

	// Paged reads return the four records exactly as their append responses
	// reported them, and the cursor closes after the last page.
	page, next := decodePage(t, getEventsQuery(t, router, "limit=2"))
	wantSeqs(t, page, 1, 2)
	if next != float64(2) {
		t.Fatalf("next_after_seq = %v, want 2", next)
	}
	rest, next := decodePage(t, getEventsQuery(t, router, "limit=2&after_seq=2"))
	wantSeqs(t, rest, 3, 4)
	if next != nil {
		t.Fatalf("next_after_seq = %v, want null on the last page", next)
	}
	wantEventsMatchPosted(t, append(page, rest...), posted)
}

func TestVerifyInFlightStopsAtPreExistingBreakWhenAppendLands(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st := openStoreAt(t, path)
	defer closeStore(t, st)
	router := NewRouter(st)
	posted := seedEvents(t, router, 3)

	// The second record keeps its business fields, seq and self-hash, but
	// its stored back-link no longer matches the first record.
	corrupt(t, path, "UPDATE events SET prev_hash = ? WHERE seq = 2", strings.Repeat("0", 64))

	verify := startPausedVerify(t, router)
	verify.awaitFirstRow(t)
	verify.assertPending(t)

	// The fourth record still commits while the pass is parked: the broken
	// back-link at seq 2 does not block appends at the tail.
	fourth := mustPost(t, router, eventBody("audit", "2026-10-05T00:03:00Z"))
	if fourth["seq"] != float64(4) {
		t.Fatalf("fourth seq = %v, want 4", fourth["seq"])
	}
	if fourth["prev_hash"] != posted[2]["hash"] {
		t.Fatalf("fourth prev_hash = %v, want the third record's hash %v", fourth["prev_hash"], posted[2]["hash"])
	}
	verify.assertPending(t)

	// The pass stops at the pre-existing break: the committed record
	// neither skips it nor extends the checked count past it.
	verify.unblock()
	recorder := verify.result(t)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
	body := decodeVerify(t, recorder)
	if body.Valid || body.Checked != 2 || body.FirstInvalidSeq == nil || *body.FirstInvalidSeq != 2 {
		t.Fatalf("in-flight verify = %+v, want invalid/2/first 2", body)
	}
	assertVerifyExactFields(t, recorder)

	// Every later pass reports the same break at the same position.
	recorder = getLedgerVerifyRequest(t, router, "/ledger/verify")
	if recorder.Code != http.StatusOK {
		t.Fatalf("re-verify status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
	body = decodeVerify(t, recorder)
	if body.Valid || body.Checked != 2 || body.FirstInvalidSeq == nil || *body.FirstInvalidSeq != 2 {
		t.Fatalf("re-verify = %+v, want invalid/2/first 2", body)
	}
	assertVerifyExactFields(t, recorder)

	// Paged reads return all four records. Records 1, 3 and 4 are exactly
	// as appended; record 2 keeps its business fields, seq and hash, with
	// only the stored back-link replaced.
	page, next := decodePage(t, getEventsQuery(t, router, "limit=4"))
	if next != nil {
		t.Fatalf("next_after_seq = %v, want null on the only page", next)
	}
	wantSeqs(t, page, 1, 2, 3, 4)
	wantEventsMatchPosted(t,
		[]map[string]any{page[0], page[2], page[3]},
		[]map[string]any{posted[0], posted[2], fourth})
	for _, key := range []string{"account", "operation", "resource", "result", "occurred_at", "seq", "hash"} {
		if page[1][key] != posted[1][key] {
			t.Fatalf("record 2 %s = %v, want the appended value %v", key, page[1][key], posted[1][key])
		}
	}
	if page[1]["prev_hash"] != strings.Repeat("0", 64) {
		t.Fatalf("record 2 prev_hash = %v, want the corrupted back-link", page[1]["prev_hash"])
	}
}

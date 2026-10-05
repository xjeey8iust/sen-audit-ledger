package api

// Regression tests for a storage layer that stays connected — GET /healthz
// keeps reporting ok and appends keep committing — but fails a read partway
// through, after a valid prefix was already scanned.
//
// The scenarios below assert that:
//
//   - GET /events fails within the rows this request still has to read, after
//     a valid prefix was scanned, and answers 503 with only the bare error
//     object: no events, no next_after_seq;
//   - GET /ledger/verify reads a fully legal prefix and fails on a later row,
//     with the same bare 503: no valid, checked or first_invalid_seq;
//   - neither error body echoes SQL, stack frames or file paths;
//   - GET /healthz still reports its normal ok state during the fault;
//   - input validation (limit=0; any verify query parameter) still wins with
//     400 invalid_audit_input and never reaches the failing read;
//   - verification stops at the second record when that record chains badly,
//     before a read fault planted on later rows can be reached;
//   - once the fault is lifted the untouched ledger pages back in full with a
//     null terminal cursor, verifies as a whole, and the next append continues
//     straight from the original tail.
//
// A write failure can be injected purely through a SQLite trigger (see
// setInsertFailure), but no equivalent SQL mechanism fails a single read after
// some rows were already scanned — the modernc driver silently coerces BLOBs
// into strings, every events column is NOT NULL and the seq column is an
// INTEGER PRIMARY KEY. The store therefore carries a nil test seam
// (readFaultHook); the link below reaches that unexported seam without adding
// any exported production surface, storage-format or documented change. The
// seam is a no-op nil in production and is never consulted by Ping or writes.

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	_ "unsafe" // for go:linkname against the store's test seam
)

//go:linkname readFaultHook github.com/xjeey8iust/sen-audit-ledger/internal/store.readFaultHook
var readFaultHook func(rowsRead int) error

// errInjectedRead deliberately looks like an internal database error so tests
// can prove such text never reaches an HTTP response.
var errInjectedRead = errors.New("injected mid-read failure: SELECT scan failed at /srv/ledger/service.db (store.go:1)")

// readFaultState records how far a faulted read actually progressed.
type readFaultState struct {
	calls   int // number of scanned rows the hook saw
	fired   int // number of times the hook returned an error
	ordinal int // ordinal of the last scanned row
}

// installReadFaultAfter lets a pass scan a valid prefix of prefixLength rows,
// then fails the read while examining the next row. The cleanup restores the
// nil hook even if the test fails early.
func installReadFaultAfter(t *testing.T, prefixLength int) *readFaultState {
	t.Helper()
	state := &readFaultState{}
	readFaultHook = func(rowsRead int) error {
		state.calls++
		state.ordinal = rowsRead
		if rowsRead > prefixLength {
			state.fired++
			return errInjectedRead
		}
		return nil
	}
	t.Cleanup(func() { readFaultHook = nil })
	return state
}

func clearInstalledReadFault() { readFaultHook = nil }

// assertStorageError503 asserts the published storage error shape and, in
// addition, that no endpoint-specific partial-result field accompanies it.
func assertStorageError503(t *testing.T, recorder *httptest.ResponseRecorder, forbiddenFields ...string) {
	t.Helper()
	assertErrorEnvelope(t, recorder, http.StatusServiceUnavailable, "storage_unavailable")
	body := recorder.Body.String()
	for _, field := range forbiddenFields {
		if strings.Contains(body, field) {
			t.Fatalf("storage error carries partial field %q: %s", field, body)
		}
	}
	if strings.Contains(body, "injected") {
		t.Fatalf("storage error leaks the injected internal error: %s", body)
	}
}

// eventResponseFields are the eight fields every persisted event carries.
var eventResponseFields = []string{
	"account", "operation", "resource", "result", "occurred_at", "seq", "prev_hash", "hash",
}

// assertEventEqual asserts one read-back event is identical, field for field,
// to the response captured when the record was appended.
func assertEventEqual(t *testing.T, got, want map[string]any, context string) {
	t.Helper()
	if len(got) != len(eventResponseFields) {
		t.Fatalf("%s: event keys = %v, want the 8 published fields", context, got)
	}
	for _, key := range eventResponseFields {
		if got[key] != want[key] {
			t.Fatalf("%s: %s = %v, want %v", context, key, got[key], want[key])
		}
	}
}

// assertPhysicalLedger reads the database file through a connection the
// service does not own and proves the physical rows are exactly the captured
// append responses: contiguous seqs, verbatim business fields, back-links and
// hashes. It is the on-disk counterpart to the API-level page comparisons.
func assertPhysicalLedger(t *testing.T, path string, seeded []map[string]any) {
	t.Helper()
	physical := readFullLedger(t, path)
	if len(physical) != len(seeded) {
		t.Fatalf("physical ledger has %d rows, want %d", len(physical), len(seeded))
	}
	prevHash := strings.Repeat("0", 64)
	for i, want := range seeded {
		row := physical[i]
		if row.seq != int64(i+1) {
			t.Fatalf("physical row %d seq = %d, want %d", i, row.seq, i+1)
		}
		pairs := []struct {
			key string
			got string
		}{
			{"account", row.account}, {"operation", row.operation}, {"resource", row.resource},
			{"result", row.result}, {"occurred_at", row.occurredAt},
		}
		for _, p := range pairs {
			if p.got != want[p.key].(string) {
				t.Fatalf("physical row %d %s = %q, want %q", i, p.key, p.got, want[p.key])
			}
		}
		if row.prevHash != prevHash {
			t.Fatalf("physical row %d prev_hash = %s, want %s", i, row.prevHash, prevHash)
		}
		if row.hash != want["hash"].(string) {
			t.Fatalf("physical row %d hash = %s, want %s", i, row.hash, want["hash"])
		}
		prevHash = row.hash
	}
}

// TestReadFailureAfterValidPrefix covers the full fault lifecycle against one
// isolated temporary ledger: seed legal records, fault reads after a prefix,
// observe bare 503s while healthz stays ok and validation stays 400, then lift
// the fault and prove paging, verification, data and append chaining recover.
func TestReadFailureAfterValidPrefix(t *testing.T) {
	const recordCount = 4

	path := filepath.Join(t.TempDir(), "service.db")
	st := openStoreAt(t, path)
	defer closeStore(t, st)
	router := NewRouter(st)

	// Seed at least three consecutive legal records and keep every 201 body:
	// those captures are the reference for every later read.
	seeded := make([]map[string]any, recordCount)
	for i := range seeded {
		seeded[i] = mustPost(t, router, eventBody("alice", fmt.Sprintf("2026-10-05T00:0%d:00Z", i)))
	}

	// A read before the fault returns every record with a closed cursor, each
	// already identical to its append response.
	events, next := decodePage(t, getEventsQuery(t, router, ""))
	if len(events) != recordCount || next != nil {
		t.Fatalf("pre-fault page = %d events, next = %v; want %d and null", len(events), next, recordCount)
	}
	for i, want := range seeded {
		assertEventEqual(t, events[i], want, "pre-fault page")
	}
	// The on-disk rows match the 201 responses before any fault is armed.
	assertPhysicalLedger(t, path, seeded)

	// Fault both reads after a valid two-record prefix; the third row is still
	// inside what each request needs to read.
	state := installReadFaultAfter(t, 2)

	// Pagination: the failing row is inside this request's range (default
	// limit 50), so the answer is a bare 503 with no partial page.
	assertStorageError503(t, getEventsQuery(t, router, ""), "events", "next_after_seq")

	// Verification: records one and two are fully legal, then the read fails;
	// no partial verification may accompany the error.
	assertStorageError503(t, getLedgerVerifyRequest(t, router, "/ledger/verify"),
		"valid", "checked", "first_invalid_seq")

	// The reads genuinely scanned a two-row prefix and failed examining the
	// third row — this is an in-progress read failure, not a connect failure.
	if state.fired < 2 {
		t.Fatalf("read fault fired %d times, want once per read", state.fired)
	}
	if state.ordinal != 3 {
		t.Fatalf("fault last fired at ordinal %d, want 3 (after a 2-row prefix)", state.ordinal)
	}

	// The database is still connected: health keeps its normal ok response.
	if health := getHealthz(t, router); health.Code != http.StatusOK {
		t.Fatalf("healthz during fault: status = %d (%s)", health.Code, health.Body.String())
	} else if got := health.Body.String(); got != `{"database":"ok","status":"ok"}` {
		t.Fatalf("healthz body = %s, want the unchanged ok body", got)
	}

	// Under the same fault, input validation still happens before storage and
	// never reaches the read.
	callsBeforeValidation := state.calls
	assertErrorEnvelope(t, getEventsQuery(t, router, "limit=0"), http.StatusBadRequest, "invalid_audit_input")
	assertErrorEnvelope(t, getLedgerVerifyRequest(t, router, "/ledger/verify?x=1"), http.StatusBadRequest, "invalid_audit_input")
	if state.calls != callsBeforeValidation {
		t.Fatalf("validation touched storage: hook calls went from %d to %d", callsBeforeValidation, state.calls)
	}

	// Lift the fault: the failed reads never modified the ledger.
	clearInstalledReadFault()

	// Paging returns all original records in order across pages; the last page
	// carrying original data closes the cursor with null.
	events, nextCursor := decodePage(t, getEventsQuery(t, router, "limit=2"))
	wantSeqs(t, events, 1, 2)
	if nextCursor != float64(2) {
		t.Fatalf("page 1 next = %v, want 2", nextCursor)
	}
	for i := 0; i < 2; i++ {
		assertEventEqual(t, events[i], seeded[i], "recovered page 1")
	}
	events, nextCursor = decodePage(t, getEventsQuery(t, router, "limit=2&after_seq=2"))
	wantSeqs(t, events, 3, 4)
	if nextCursor != nil {
		t.Fatalf("page 2 next = %v, want null on the final data page", nextCursor)
	}
	for i := 2; i < recordCount; i++ {
		assertEventEqual(t, events[i-2], seeded[i], "recovered page 2")
	}
	// A cursor past the end is an empty page that also closes with null.
	events, nextCursor = decodePage(t, getEventsQuery(t, router, "after_seq=4"))
	if len(events) != 0 || nextCursor != nil {
		t.Fatalf("terminal page = %d events, next = %v; want empty and null", len(events), nextCursor)
	}
	// The failed reads changed nothing on disk: the physical rows are still
	// exactly the four captured append responses.
	assertPhysicalLedger(t, path, seeded)

	// Whole-ledger verification is valid again for the full record count.
	verifyRecorder := getLedgerVerifyRequest(t, router, "/ledger/verify")
	if verifyRecorder.Code != http.StatusOK {
		t.Fatalf("recovered verify: status = %d (%s)", verifyRecorder.Code, verifyRecorder.Body.String())
	}
	body := decodeVerify(t, verifyRecorder)
	if !body.Valid || body.Checked != recordCount || body.FirstInvalidSeq != nil {
		t.Fatalf("recovered verify = %+v, want valid/%d/null", body, recordCount)
	}
	assertVerifyExactFields(t, verifyRecorder)

	// The next append is 201 and continues directly from the original tail.
	lastSeed := seeded[recordCount-1]
	appended := mustPost(t, router, eventBody("alice", "2026-10-05T00:04:00Z"))
	if appended["seq"] != float64(recordCount+1) {
		t.Fatalf("appended seq = %v, want %d", appended["seq"], recordCount+1)
	}
	if appended["prev_hash"] != lastSeed["hash"] {
		t.Fatalf("appended prev_hash = %v, want original tail %v", appended["prev_hash"], lastSeed["hash"])
	}

	// The extended chain verifies as a whole and the new tail reads back with a
	// closed cursor, field-for-field equal to its append response.
	verifyRecorder = getLedgerVerifyRequest(t, router, "/ledger/verify")
	body = decodeVerify(t, verifyRecorder)
	if !body.Valid || body.Checked != recordCount+1 || body.FirstInvalidSeq != nil {
		t.Fatalf("post-append verify = %+v, want valid/%d/null", body, recordCount+1)
	}
	events, nextCursor = decodePage(t, getEventsQuery(t, router, "after_seq=4"))
	if len(events) != 1 || nextCursor != nil {
		t.Fatalf("tail page = %d events, next = %v; want 1 and null", len(events), nextCursor)
	}
	assertEventEqual(t, events[0], appended, "tail page after recovery")

	// On disk the extended chain is the four originals followed by the new
	// row, each chained to the captured hash of its predecessor.
	assertPhysicalLedger(t, path, append(append([]map[string]any{}, seeded...), appended))
}

// TestVerifyStopsAtBrokenRecordBeforeLaterReadFault plants a bad back-link on
// record two (its seq stays consecutive) and arms a read fault for later
// records. Verification must stop at record two with 200/invalid/checked
// 2/first_invalid_seq 2 and never advance far enough to trip the fault.
func TestVerifyStopsAtBrokenRecordBeforeLaterReadFault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st := openStoreAt(t, path)
	defer closeStore(t, st)
	router := NewRouter(st)

	for i := 0; i < 4; i++ {
		if recorder := postEventBody(t, router, validEventBody()); recorder.Code != http.StatusCreated {
			t.Fatalf("append %d failed: %s", i+1, recorder.Body.String())
		}
	}

	// Record two keeps its consecutive seq but no longer points at record one.
	corrupt(t, path, "UPDATE events SET prev_hash = ? WHERE seq = 2", strings.Repeat("0", 64))

	// Later records carry a read fault: examining row three would fail.
	state := installReadFaultAfter(t, 2)

	recorder := getLedgerVerifyRequest(t, router, "/ledger/verify")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
	verify := decodeVerify(t, recorder)
	if verify.Valid || verify.Checked != 2 {
		t.Fatalf("verify = %+v, want invalid with checked 2", verify)
	}
	if verify.FirstInvalidSeq == nil || *verify.FirstInvalidSeq != 2 {
		t.Fatalf("first_invalid_seq = %v, want 2", verify.FirstInvalidSeq)
	}
	assertVerifyExactFields(t, recorder)

	// The pass stopped at record two and never read the faulted later rows.
	if state.fired != 0 {
		t.Fatalf("read fault fired %d times; verification must stop at record 2", state.fired)
	}
	if state.ordinal > 2 {
		t.Fatalf("verification read as far as row %d; want it to stop at 2", state.ordinal)
	}

	// The fault is genuinely armed for reads that continue past record two.
	assertStorageError503(t, getEventsQuery(t, router, "limit=50"), "events", "next_after_seq")
	if state.fired == 0 {
		t.Fatal("read fault did not fire for a full ledger listing")
	}
}

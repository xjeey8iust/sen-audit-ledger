package api

// Regression tests for read failures that strike mid-scan while the database
// itself stays reachable (unlike the closed-handle cases, which fail before
// the first row):
//
//   - GET /events and GET /ledger/verify read a valid record prefix, then hit
//     a storage error on a later row; both answer 503 storage_unavailable
//     with a bare top-level error object and no partial result fields;
//   - invalid input (limit=0, verify query parameters) still wins a 400
//     invalid_audit_input over the storage failure, in the same body shape;
//   - GET /healthz keeps reporting ok throughout the read fault;
//   - a broken back-link at seq 2 stops verification there even when a read
//     fault waits at seq 3;
//   - once the ledger and the read path are restored, pagination returns the
//     original records ending in a null cursor, verification reports a valid
//     full chain, and appends still chain onto the original tail.
//
// The fault is injected by renaming the events table and putting a view in
// its place that serves rows below a cutoff and raises an error when the scan
// reaches it, so the database stays open and healthy the whole time.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// failReadsAt swaps the events table for a view that serves rows with seq
// below failAt unchanged and raises a storage error when the scan reaches
// failAt. The database stays open and answers pings.
func failReadsAt(t *testing.T, path string, failAt int64) {
	t.Helper()
	corrupt(t, path, "ALTER TABLE events RENAME TO events_data")
	corrupt(t, path, fmt.Sprintf(
		"CREATE VIEW events AS SELECT seq, account, operation, resource, result, occurred_at, prev_hash, "+
			"CASE WHEN seq >= %d THEN json('[') ELSE hash END AS hash FROM events_data", failAt))
}

// restoreReads undoes failReadsAt, putting the real table back in place.
func restoreReads(t *testing.T, path string) {
	t.Helper()
	corrupt(t, path, "DROP VIEW events")
	corrupt(t, path, "ALTER TABLE events_data RENAME TO events")
}

// assertFaultReadsPrefix confirms through an independent connection that the
// injected fault really serves exactly the wantSeqs prefix before the read
// fails, so the 503s above exercise a mid-scan failure, not an early one.
func assertFaultReadsPrefix(t *testing.T, path string, wantSeqs ...int64) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open control connection: %v", err)
	}
	defer db.Close()

	// Select the hash column too: the fault lives in its CASE expression, so
	// a seq-only scan would never evaluate it.
	rows, err := db.Query("SELECT seq, hash FROM events ORDER BY seq ASC")
	if err != nil {
		t.Fatalf("query through fault view: %v", err)
	}
	var got []int64
	for rows.Next() {
		var seq int64
		var hash string
		if err := rows.Scan(&seq, &hash); err != nil {
			t.Fatalf("scan through fault view: %v", err)
		}
		got = append(got, seq)
	}
	iterErr := rows.Err()
	rows.Close()
	if iterErr == nil {
		t.Fatalf("fault view returned all rows without an error; want a failure after %v", wantSeqs)
	}
	if !reflect.DeepEqual(got, wantSeqs) {
		t.Fatalf("fault view served seqs %v before failing, want %v", got, wantSeqs)
	}
}

// seedEvents appends n valid records and returns their 201 response bodies.
func seedEvents(t *testing.T, router http.Handler, n int) []map[string]any {
	t.Helper()
	posted := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		posted = append(posted, mustPost(t, router, eventBody("audit", fmt.Sprintf("2026-10-05T00:0%d:00Z", i))))
	}
	return posted
}

// wantEventsMatchPosted fails unless every read-back record carries the same
// business fields, seq and chain values as its append response.
func wantEventsMatchPosted(t *testing.T, events, posted []map[string]any) {
	t.Helper()
	if len(events) != len(posted) {
		t.Fatalf("read back %d events, want %d (%v)", len(events), len(posted), events)
	}
	for i := range posted {
		for _, key := range []string{"account", "operation", "resource", "result", "occurred_at", "seq", "prev_hash", "hash"} {
			if events[i][key] != posted[i][key] {
				t.Fatalf("event %d %s = %v, want %v", i, key, events[i][key], posted[i][key])
			}
		}
	}
}

// wantStorageError asserts the 503 storage_unavailable shape: a single
// top-level error object with a non-empty message, none of the endpoint's
// partial result keys, and no SQL, stack or path leakage.
func wantStorageError(t *testing.T, recorder *httptest.ResponseRecorder, forbiddenKeys ...string) {
	t.Helper()
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (%s)", recorder.Code, recorder.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode error body: %v (%s)", err, recorder.Body.String())
	}
	if len(raw) != 1 {
		t.Fatalf("top-level keys = %v, want only the error object", raw)
	}
	var decoded struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode error body: %v (%s)", err, recorder.Body.String())
	}
	if decoded.Error.Code != "storage_unavailable" {
		t.Fatalf("code = %q, want storage_unavailable", decoded.Error.Code)
	}
	if decoded.Error.Message == "" {
		t.Fatal("message must be a non-empty string")
	}
	body := recorder.Body.String()
	for _, key := range forbiddenKeys {
		if strings.Contains(body, key) {
			t.Fatalf("503 body carries partial result key %q: %s", key, body)
		}
	}
	for _, leak := range []string{"SELECT", "sql", ".go", "goroutine", "/", "\\"} {
		if strings.Contains(body, leak) {
			t.Fatalf("error body leaks %q: %s", leak, body)
		}
	}
}

// wantInvalidAuditInput asserts the 400 invalid_audit_input shape: a single
// top-level error object with a non-empty message.
func wantInvalidAuditInput(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", recorder.Code, recorder.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode error body: %v (%s)", err, recorder.Body.String())
	}
	if len(raw) != 1 {
		t.Fatalf("top-level keys = %v, want only the error object", raw)
	}
	var decoded struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode error body: %v (%s)", err, recorder.Body.String())
	}
	if decoded.Error.Code != "invalid_audit_input" {
		t.Fatalf("code = %q, want invalid_audit_input", decoded.Error.Code)
	}
	if decoded.Error.Message == "" {
		t.Fatal("message must be a non-empty string")
	}
}

// wantHealthy asserts GET /healthz keeps its normal ok response.
func wantHealthy(t *testing.T, router http.Handler) {
	t.Helper()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != `{"database":"ok","status":"ok"}` {
		t.Fatalf("healthz = %d %s, want the normal ok body", recorder.Code, recorder.Body.String())
	}
}

func TestGetEventsReadFailureAfterPrefixReturns503(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st := openStoreAt(t, path)
	defer closeStore(t, st)
	router := NewRouter(st)
	posted := seedEvents(t, router, 4)

	// Baseline: the intact ledger pages through all four records, and the
	// read-back fields match the append responses.
	page, next := decodePage(t, getEventsQuery(t, router, "limit=2"))
	wantSeqs(t, page, 1, 2)
	if next != float64(2) {
		t.Fatalf("next_after_seq = %v, want 2", next)
	}
	second, next := decodePage(t, getEventsQuery(t, router, "limit=2&after_seq=2"))
	wantSeqs(t, second, 3, 4)
	if next != nil {
		t.Fatalf("next_after_seq = %v, want null on the last page", next)
	}
	wantEventsMatchPosted(t, append(page, second...), posted)

	// Reads fail at seq 3 after serving the valid prefix; the database stays
	// up. With limit=2 the request must still read seq 3 to size the page.
	failReadsAt(t, path, 3)
	assertFaultReadsPrefix(t, path, 1, 2)

	recorder := getEventsQuery(t, router, "limit=2")
	wantStorageError(t, recorder, "events", "next_after_seq")

	// Only the read path is broken: healthz still reports ok.
	wantHealthy(t, router)

	// Invalid input is still a 400 before storage is touched, in the same
	// error body shape.
	wantInvalidAuditInput(t, getEventsQuery(t, router, "limit=0"))

	// With the read path restored, pagination returns every original record
	// again and the last page closes the cursor.
	restoreReads(t, path)
	page, next = decodePage(t, getEventsQuery(t, router, "limit=2"))
	wantSeqs(t, page, 1, 2)
	if next != float64(2) {
		t.Fatalf("next_after_seq = %v, want 2", next)
	}
	second, next = decodePage(t, getEventsQuery(t, router, "limit=2&after_seq=2"))
	wantSeqs(t, second, 3, 4)
	if next != nil {
		t.Fatalf("next_after_seq = %v, want null on the last page", next)
	}
	wantEventsMatchPosted(t, append(page, second...), posted)

	// Appends still chain onto the original tail.
	fifth := mustPost(t, router, eventBody("audit", "2026-10-05T00:04:00Z"))
	if fifth["seq"] != float64(5) || fifth["prev_hash"] != posted[3]["hash"] {
		t.Fatalf("fifth = %v, want seq 5 chained to %s", fifth, posted[3]["hash"])
	}
}

func TestVerifyReadFailureAfterValidPrefixReturns503(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st := openStoreAt(t, path)
	defer closeStore(t, st)
	router := NewRouter(st)
	posted := seedEvents(t, router, 4)

	// Baseline: the intact chain verifies.
	recorder := getLedgerVerifyRequest(t, router, "/ledger/verify")
	if body := decodeVerify(t, recorder); !body.Valid || body.Checked != 4 || body.FirstInvalidSeq != nil {
		t.Fatalf("baseline verify = %+v, want valid/4/null", body)
	}

	// Records 1-2 are legal and read fine; the read fails at seq 3.
	failReadsAt(t, path, 3)
	assertFaultReadsPrefix(t, path, 1, 2)

	recorder = getLedgerVerifyRequest(t, router, "/ledger/verify")
	wantStorageError(t, recorder, "valid", "checked", "first_invalid_seq")

	// Only the read path is broken: healthz still reports ok.
	wantHealthy(t, router)

	// A query parameter is still a 400 before storage is touched, in the same
	// error body shape.
	wantInvalidAuditInput(t, getLedgerVerifyRequest(t, router, "/ledger/verify?detail=1"))

	// With the read path restored, the whole chain verifies again and the
	// stored records are untouched.
	restoreReads(t, path)
	recorder = getLedgerVerifyRequest(t, router, "/ledger/verify")
	if body := decodeVerify(t, recorder); !body.Valid || body.Checked != 4 || body.FirstInvalidSeq != nil {
		t.Fatalf("restored verify = %+v, want valid/4/null", body)
	}
	assertVerifyExactFields(t, recorder)
	page, next := decodePage(t, getEventsQuery(t, router, "limit=4"))
	if next != nil {
		t.Fatalf("next_after_seq = %v, want null on the only page", next)
	}
	wantEventsMatchPosted(t, page, posted)

	// Appends still chain onto the original tail.
	fifth := mustPost(t, router, eventBody("audit", "2026-10-05T00:04:00Z"))
	if fifth["seq"] != float64(5) || fifth["prev_hash"] != posted[3]["hash"] {
		t.Fatalf("fifth = %v, want seq 5 chained to %s", fifth, posted[3]["hash"])
	}
}

func TestVerifyBrokenChainStopsBeforeReadFault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st := openStoreAt(t, path)
	defer closeStore(t, st)
	router := NewRouter(st)
	posted := seedEvents(t, router, 4)

	// Seq 2 keeps its sequence position but its back-link no longer matches,
	// and a read fault waits further along at seq 3.
	corrupt(t, path, "UPDATE events SET prev_hash = ? WHERE seq = 2", strings.Repeat("0", 64))
	failReadsAt(t, path, 3)

	// Verification stops at seq 2 and never reaches the fault.
	recorder := getLedgerVerifyRequest(t, router, "/ledger/verify")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
	body := decodeVerify(t, recorder)
	if body.Valid || body.Checked != 2 || body.FirstInvalidSeq == nil || *body.FirstInvalidSeq != 2 {
		t.Fatalf("body = %+v, want invalid/2/first 2", body)
	}

	// Repair the record and the read path: the original chain verifies.
	corrupt(t, path, "UPDATE events_data SET prev_hash = ? WHERE seq = 2", posted[1]["prev_hash"])
	restoreReads(t, path)
	recorder = getLedgerVerifyRequest(t, router, "/ledger/verify")
	if body := decodeVerify(t, recorder); !body.Valid || body.Checked != 4 || body.FirstInvalidSeq != nil {
		t.Fatalf("restored verify = %+v, want valid/4/null", body)
	}

	// Appends still chain onto the original tail.
	fifth := mustPost(t, router, eventBody("audit", "2026-10-05T00:04:00Z"))
	if fifth["seq"] != float64(5) || fifth["prev_hash"] != posted[3]["hash"] {
		t.Fatalf("fifth = %v, want seq 5 chained to %s", fifth, posted[3]["hash"])
	}
}

package api

// Regression tests for GET /ledger/verify:
//
//   - an empty ledger verifies as valid with checked 0 and a null
//     first_invalid_seq;
//   - a fully chained ledger (including records with Chinese text, quotes,
//     backslashes and control characters) verifies with checked equal to the
//     record count;
//   - any query parameter is rejected with 400 invalid_audit_input, ahead of
//     any storage failure;
//   - a tampered hash, a broken prev_hash link or a sequence gap stops the
//     check at the first offending record, and a ledger reduced to its valid
//     prefix verifies again;
//   - an unreadable database surfaces as 503 storage_unavailable with no
//     partial result.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

)

// verifyResultBody is the typed 200 response body of GET /ledger/verify.
type verifyResultBody struct {
	Valid           bool   `json:"valid"`
	Checked         int64  `json:"checked"`
	FirstInvalidSeq *int64 `json:"first_invalid_seq"`
}

func requestVerify(t *testing.T, router http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
	return recorder
}

// decodeVerifyResult checks the 200 envelope shape: exactly the three
// published fields, with first_invalid_seq either null or an integer.
func decodeVerifyResult(t *testing.T, recorder *httptest.ResponseRecorder) verifyResultBody {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (%s)", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode verify body: %v (%s)", err, recorder.Body.String())
	}
	if len(raw) != 3 {
		t.Fatalf("top-level keys = %v, want exactly valid, checked, first_invalid_seq", raw)
	}
	for _, key := range []string{"valid", "checked", "first_invalid_seq"} {
		if _, present := raw[key]; !present {
			t.Fatalf("missing key %q in %s", key, recorder.Body.String())
		}
	}
	var got verifyResultBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode typed verify body: %v (%s)", err, recorder.Body.String())
	}
	return got
}

// seedChain appends count well-formed events and returns the router and the
// persisted chain read back through an independent connection.
func seedChain(t *testing.T, path string, fixtures []eventFixture) []persistedEvent {
	t.Helper()
	st := openStoreAt(t, path)
	router := NewRouter(st)
	for i, fx := range fixtures {
		recorder := postStructuredEvent(t, router, fx.req)
		if recorder.Code != http.StatusCreated {
			t.Fatalf("seed %d: status = %d (%s)", i, recorder.Code, recorder.Body.String())
		}
	}
	closeStore(t, st)
	return readFullLedger(t, path)
}

// verifyFixtures covers the text shapes the hash rule must preserve: Chinese
// characters, a double quote, a backslash, control characters and fractional
// seconds in several shapes.
func verifyFixtures() []eventFixture {
	return []eventFixture{
		{eventRequest{Account: "用户\"甲\"", Operation: "login", Resource: `console\01`, Result: "ok", OccurredAt: "2026-10-05T08:30:00.5+08:00"}, "2026-10-05T00:30:00.5Z"},
		{eventRequest{Account: "line\nbreak\ttab", Operation: "read", Resource: "报表/<资源>", Result: "denied", OccurredAt: "2026-10-05T01:00:00.123456789Z"}, "2026-10-05T01:00:00.123456789Z"},
		{eventRequest{Account: "carol", Operation: "write", Resource: "bucket", Result: "ok", OccurredAt: "2026-03-01T00:15:00.25+05:30"}, "2026-02-28T18:45:00.25Z"},
		{eventRequest{Account: "dave", Operation: "logout", Resource: "console", Result: "ok", OccurredAt: "2026-10-05T02:00:00Z"}, "2026-10-05T02:00:00Z"},
	}
}

// tamperLedger runs one UPDATE against the events table through a connection
// the service does not own.
func tamperLedger(t *testing.T, path, statement string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open control connection: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(statement, args...); err != nil {
		t.Fatalf("tamper: %v", err)
	}
}

func TestVerifyEmptyLedgerIsValid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st := openStoreAt(t, path)
	defer closeStore(t, st)

	recorder := requestVerify(t, NewRouter(st), "/ledger/verify")
	got := decodeVerifyResult(t, recorder)
	if !got.Valid || got.Checked != 0 || got.FirstInvalidSeq != nil {
		t.Fatalf("empty ledger = %+v, want valid checked 0 null first_invalid_seq", got)
	}
}

func TestVerifyValidChainCoversEveryRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	fixtures := verifyFixtures()
	seedChain(t, path, fixtures)

	st := openStoreAt(t, path)
	defer closeStore(t, st)
	recorder := requestVerify(t, NewRouter(st), "/ledger/verify")
	got := decodeVerifyResult(t, recorder)
	if !got.Valid || got.Checked != int64(len(fixtures)) || got.FirstInvalidSeq != nil {
		t.Fatalf("valid chain = %+v, want valid checked %d null first_invalid_seq", got, len(fixtures))
	}
}

func TestVerifyRejectsAnyQueryParameter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st := openStoreAt(t, path)
	router := NewRouter(st)

	for _, target := range []string{
		"/ledger/verify?from=1",
		"/ledger/verify?from=1&to=9",
		"/ledger/verify?detailed",
	} {
		recorder := requestVerify(t, router, target)
		assertErrorEnvelope(t, recorder, http.StatusBadRequest, "invalid_audit_input")
	}

	// The 400 takes precedence over a storage failure.
	closeStore(t, st)
	recorder := requestVerify(t, router, "/ledger/verify?from=1")
	assertErrorEnvelope(t, recorder, http.StatusBadRequest, "invalid_audit_input")
}

func TestVerifyDetectsTamperedHash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	seedChain(t, path, verifyFixtures())

	// Corrupt the stored hash of record 3 only.
	tamperLedger(t, path, "UPDATE events SET hash = ? WHERE seq = 3", strings.Repeat("f", 64))

	st := openStoreAt(t, path)
	defer closeStore(t, st)
	recorder := requestVerify(t, NewRouter(st), "/ledger/verify")
	got := decodeVerifyResult(t, recorder)
	if got.Valid || got.Checked != 3 || got.FirstInvalidSeq == nil || *got.FirstInvalidSeq != 3 {
		t.Fatalf("tampered hash = %+v, want invalid checked 3 first_invalid_seq 3", got)
	}
}

func TestVerifyDetectsBrokenPrevHashLink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	seedChain(t, path, verifyFixtures())

	// Rewrite record 2's prev_hash so it no longer matches record 1's hash.
	tamperLedger(t, path, "UPDATE events SET prev_hash = ? WHERE seq = 2", strings.Repeat("a", 64))

	st := openStoreAt(t, path)
	defer closeStore(t, st)
	recorder := requestVerify(t, NewRouter(st), "/ledger/verify")
	got := decodeVerifyResult(t, recorder)
	if got.Valid || got.Checked != 2 || got.FirstInvalidSeq == nil || *got.FirstInvalidSeq != 2 {
		t.Fatalf("broken link = %+v, want invalid checked 2 first_invalid_seq 2", got)
	}
}

func TestVerifyDetectsTamperedBusinessField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	seedChain(t, path, verifyFixtures())

	// Editing a business field invalidates the record's own hash.
	tamperLedger(t, path, "UPDATE events SET result = 'ok' WHERE seq = 2")

	st := openStoreAt(t, path)
	defer closeStore(t, st)
	recorder := requestVerify(t, NewRouter(st), "/ledger/verify")
	got := decodeVerifyResult(t, recorder)
	if got.Valid || got.Checked != 2 || got.FirstInvalidSeq == nil || *got.FirstInvalidSeq != 2 {
		t.Fatalf("tampered field = %+v, want invalid checked 2 first_invalid_seq 2", got)
	}
}

func TestVerifyDetectsSequenceGap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	seedChain(t, path, verifyFixtures())

	// Remove record 2: record 3 now sits where seq 2 is expected.
	tamperLedger(t, path, "DELETE FROM events WHERE seq = 2")

	st := openStoreAt(t, path)
	defer closeStore(t, st)
	recorder := requestVerify(t, NewRouter(st), "/ledger/verify")
	got := decodeVerifyResult(t, recorder)
	// checked counts the intact record 1 plus the existing record that
	// triggered the failure; first_invalid_seq is the expected seq, not the
	// stored one.
	if got.Valid || got.Checked != 2 || got.FirstInvalidSeq == nil || *got.FirstInvalidSeq != 2 {
		t.Fatalf("sequence gap = %+v, want invalid checked 2 first_invalid_seq 2", got)
	}
}

func TestVerifyValidPrefixLedgerPasses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	fixtures := verifyFixtures()
	seedChain(t, path, fixtures)

	// Tamper with the tail, then drop everything from the first bad record
	// on: the remaining valid prefix must verify as a complete chain.
	tamperLedger(t, path, "UPDATE events SET hash = ? WHERE seq = 3", strings.Repeat("f", 64))
	tamperLedger(t, path, "DELETE FROM events WHERE seq >= 3")

	st := openStoreAt(t, path)
	defer closeStore(t, st)
	recorder := requestVerify(t, NewRouter(st), "/ledger/verify")
	got := decodeVerifyResult(t, recorder)
	if !got.Valid || got.Checked != 2 || got.FirstInvalidSeq != nil {
		t.Fatalf("valid prefix = %+v, want valid checked 2 null first_invalid_seq", got)
	}
}

func TestVerifyReportsStorageUnavailable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	seedChain(t, path, verifyFixtures()[:2])

	st := openStoreAt(t, path)
	router := NewRouter(st)
	closeStore(t, st)

	recorder := requestVerify(t, router, "/ledger/verify")
	assertErrorEnvelope(t, recorder, http.StatusServiceUnavailable, "storage_unavailable")
}

func TestVerifyDoesNotModifyLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	fixtures := verifyFixtures()
	before := seedChain(t, path, fixtures)

	st := openStoreAt(t, path)
	router := NewRouter(st)
	for i := 0; i < 3; i++ {
		recorder := requestVerify(t, router, "/ledger/verify")
		if got := decodeVerifyResult(t, recorder); !got.Valid {
			t.Fatalf("run %d: %+v, want valid", i, got)
		}
	}
	closeStore(t, st)

	after := readFullLedger(t, path)
	if fmt.Sprintf("%+v", before) != fmt.Sprintf("%+v", after) {
		t.Fatalf("verify changed the ledger:\nbefore: %+v\nafter:  %+v", before, after)
	}
}

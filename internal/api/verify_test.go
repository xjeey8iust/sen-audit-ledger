package api

// Regression tests for GET /ledger/verify:
//
//   - an empty ledger and a complete, append-built chain report valid with
//     checked equal to the record count and first_invalid_seq null;
//   - any query string is rejected with 400 invalid_audit_input before the
//     store is touched;
//   - the first broken position stops the pass: a seq gap reports the
//     expected seq, a broken back-link or self-hash reports the record seq,
//     and checked includes the triggering record;
//   - verification never repairs stored values;
//   - passes racing concurrent appends see one committed snapshot each and
//     never report a false break;
//   - storage failure maps to 503 storage_unavailable with no partial body.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// verifyBody is the typed 200 response of GET /ledger/verify.
type verifyBody struct {
	Valid           bool   `json:"valid"`
	Checked         int64  `json:"checked"`
	FirstInvalidSeq *int64 `json:"first_invalid_seq"`
}

func getLedgerVerifyRequest(t *testing.T, router http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
	return recorder
}

func decodeVerify(t *testing.T, recorder *httptest.ResponseRecorder) verifyBody {
	t.Helper()
	var body verifyBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode verify body: %v (%s)", err, recorder.Body.String())
	}
	return body
}

// assertVerifyExactFields fails unless the body contains exactly the three
// published fields.
func assertVerifyExactFields(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode verify body: %v (%s)", err, recorder.Body.String())
	}
	if len(raw) != 3 {
		t.Fatalf("response keys = %v, want valid/checked/first_invalid_seq only", raw)
	}
	for _, key := range []string{"valid", "checked", "first_invalid_seq"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("response missing %q: %s", key, recorder.Body.String())
		}
	}
}

// corrupt opens an independent connection to tamper with committed rows in
// ways the append API can never produce.
func corrupt(t *testing.T, path string, query string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open control connection: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("corrupt (%s): %v", query, err)
	}
}

func TestVerifyEmptyLedger(t *testing.T) {
	router := NewRouter(openTestStore(t))

	recorder := getLedgerVerifyRequest(t, router, "/ledger/verify")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Body.String(); got != `{"valid":true,"checked":0,"first_invalid_seq":null}` {
		t.Fatalf("body = %s", got)
	}
	assertVerifyExactFields(t, recorder)
}

func TestVerifyCompleteChain(t *testing.T) {
	router := NewRouter(openTestStore(t))
	for i := 0; i < 4; i++ {
		if recorder := postEventBody(t, router, validEventBody()); recorder.Code != http.StatusCreated {
			t.Fatalf("append %d: status = %d (%s)", i+1, http.StatusCreated, recorder.Body.String())
		}
	}

	recorder := getLedgerVerifyRequest(t, router, "/ledger/verify")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
	body := decodeVerify(t, recorder)
	if !body.Valid || body.Checked != 4 || body.FirstInvalidSeq != nil {
		t.Fatalf("body = %+v, want valid/4/null", body)
	}
	assertVerifyExactFields(t, recorder)
}

func TestVerifyLegalPrefixAfterTailDeletion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st := openStoreAt(t, path)
	router := NewRouter(st)
	for i := 0; i < 4; i++ {
		if recorder := postEventBody(t, router, validEventBody()); recorder.Code != http.StatusCreated {
			t.Fatalf("append %d failed: %s", i+1, recorder.Body.String())
		}
	}

	// Rows 1-2 remain: a legal chain prefix is still a valid ledger.
	corrupt(t, path, "DELETE FROM events WHERE seq >= 3")

	recorder := getLedgerVerifyRequest(t, router, "/ledger/verify")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	body := decodeVerify(t, recorder)
	if !body.Valid || body.Checked != 2 || body.FirstInvalidSeq != nil {
		t.Fatalf("body = %+v, want valid/2/null", body)
	}
}

func TestVerifyRejectsAnyQueryParameter(t *testing.T) {
	// Seed one record so a storage-side pass would otherwise succeed; the 400
	// must come from input validation, not from reading the ledger.
	router := NewRouter(openTestStore(t))
	if recorder := postEventBody(t, router, validEventBody()); recorder.Code != http.StatusCreated {
		t.Fatalf("seed: %s", recorder.Body.String())
	}

	cases := map[string]string{
		"single param":   "/ledger/verify?x=1",
		"flag param":     "/ledger/verify?detail",
		"empty value":    "/ledger/verify?x=",
		"blank name":     "/ledger/verify?=1",
		"multiple":       "/ledger/verify?x=1&y=2",
		"known-looking":  "/ledger/verify?seq=1",
		"encoded value":  "/ledger/verify?x=%20",
		"fragment notit": "/ledger/verify?x#frag",
		"bare question":  "/ledger/verify?",
	}
	for name, target := range cases {
		t.Run(name, func(t *testing.T) {
			recorder := getLedgerVerifyRequest(t, router, target)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", recorder.Code, recorder.Body.String())
			}
			if event := decodeEvent(t, recorder); event["error"].(map[string]any)["code"] != "invalid_audit_input" {
				t.Fatalf("body = %v", event)
			}
			if strings.Contains(recorder.Body.String(), "SELECT") || strings.Contains(recorder.Body.String(), ".go") {
				t.Fatalf("error body leaks internals: %s", recorder.Body.String())
			}
		})
	}
}

func TestVerifyBadQueryTakesPrecedenceOverStorageFailure(t *testing.T) {
	st := openTestStore(t)
	router := NewRouter(st)
	st.Close()

	// Even with storage down, an invalid request returns 400 rather than 503.
	recorder := getLedgerVerifyRequest(t, router, "/ledger/verify?x=1")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", recorder.Code, recorder.Body.String())
	}
	if event := decodeEvent(t, recorder); event["error"].(map[string]any)["code"] != "invalid_audit_input" {
		t.Fatalf("body = %v", event)
	}
}

func TestVerifyGapReturnsExpectedSeqAndInclusiveChecked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st := openStoreAt(t, path)
	defer closeStore(t, st)
	router := NewRouter(st)
	for i := 0; i < 4; i++ {
		if recorder := postEventBody(t, router, validEventBody()); recorder.Code != http.StatusCreated {
			t.Fatalf("append %d failed: %s", i+1, recorder.Body.String())
		}
	}
	corrupt(t, path, "DELETE FROM events WHERE seq = 2") // rows 1, 3, 4 remain

	recorder := getLedgerVerifyRequest(t, router, "/ledger/verify")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	body := decodeVerify(t, recorder)
	// Row seq 1 checked and passed; row seq 3 mismatches the expected seq 2,
	// and is included in checked.
	if body.Valid || body.Checked != 2 {
		t.Fatalf("body = %+v, want invalid/2", body)
	}
	if body.FirstInvalidSeq == nil || *body.FirstInvalidSeq != 2 {
		t.Fatalf("first_invalid_seq = %v, want 2 (the expected seq)", body.FirstInvalidSeq)
	}
}

func TestVerifyMissingGenesisReturnsExpectedSeqOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st := openStoreAt(t, path)
	defer closeStore(t, st)
	router := NewRouter(st)
	for i := 0; i < 3; i++ {
		if recorder := postEventBody(t, router, validEventBody()); recorder.Code != http.StatusCreated {
			t.Fatalf("append %d failed: %s", i+1, recorder.Body.String())
		}
	}
	corrupt(t, path, "DELETE FROM events WHERE seq = 1") // rows 2, 3 remain

	recorder := getLedgerVerifyRequest(t, router, "/ledger/verify")
	body := decodeVerify(t, recorder)
	if body.Valid || body.Checked != 1 || body.FirstInvalidSeq == nil || *body.FirstInvalidSeq != 1 {
		t.Fatalf("body = %+v, want invalid/1/first 1", body)
	}
}

func TestVerifyBrokenBackLinkReturnsRecordSeq(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st := openStoreAt(t, path)
	defer closeStore(t, st)
	router := NewRouter(st)
	for i := 0; i < 4; i++ {
		if recorder := postEventBody(t, router, validEventBody()); recorder.Code != http.StatusCreated {
			t.Fatalf("append %d failed: %s", i+1, recorder.Body.String())
		}
	}
	// Point seq 3's back-link at genesis while its own hash is untouched: the
	// seqs are contiguous but the chain value is wrong.
	corrupt(t, path, "UPDATE events SET prev_hash = ? WHERE seq = 3", strings.Repeat("0", 64))

	recorder := getLedgerVerifyRequest(t, router, "/ledger/verify")
	body := decodeVerify(t, recorder)
	if body.Valid || body.Checked != 3 || body.FirstInvalidSeq == nil || *body.FirstInvalidSeq != 3 {
		t.Fatalf("body = %+v, want invalid/3/first 3", body)
	}
}

func TestVerifyTamperedSelfHashReturnsRecordSeq(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st := openStoreAt(t, path)
	defer closeStore(t, st)
	router := NewRouter(st)
	for i := 0; i < 3; i++ {
		if recorder := postEventBody(t, router, validEventBody()); recorder.Code != http.StatusCreated {
			t.Fatalf("append %d failed: %s", i+1, recorder.Body.String())
		}
	}
	corrupt(t, path, "UPDATE events SET hash = ? WHERE seq = 2", strings.Repeat("0", 64))

	recorder := getLedgerVerifyRequest(t, router, "/ledger/verify")
	body := decodeVerify(t, recorder)
	if body.Valid || body.Checked != 2 || body.FirstInvalidSeq == nil || *body.FirstInvalidSeq != 2 {
		t.Fatalf("body = %+v, want invalid/2/first 2", body)
	}

	// The tampered value must still be there: verify never writes back.
	recorder = getLedgerVerifyRequest(t, router, "/ledger/verify")
	body = decodeVerify(t, recorder)
	if body.Valid || *body.FirstInvalidSeq != 2 {
		t.Fatalf("second pass = %+v, ledger must not have been repaired", body)
	}
}

func TestVerifyTamperedTimestampFractionFailsHash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st := openStoreAt(t, path)
	defer closeStore(t, st)
	router := NewRouter(st)
	if recorder := postEventBody(t, router,
		`{"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T01:00:00.250Z"}`); recorder.Code != http.StatusCreated {
		t.Fatalf("seed: %s", recorder.Body.String())
	}
	// Change only the fractional-second text; the stored hash must no longer match.
	corrupt(t, path, "UPDATE events SET occurred_at = ? WHERE seq = 1", "2026-10-05T01:00:00.25Z")

	recorder := getLedgerVerifyRequest(t, router, "/ledger/verify")
	body := decodeVerify(t, recorder)
	if body.Valid || body.Checked != 1 || body.FirstInvalidSeq == nil || *body.FirstInvalidSeq != 1 {
		t.Fatalf("body = %+v, want invalid/1/first 1", body)
	}
}

func TestVerifySpecialCharacterChainPasses(t *testing.T) {
	router := NewRouter(openTestStore(t))
	bodies := []string{
		`{"account":"管理员\"甲","operation":"登录","resource":"控制台\\路径","result":"ok","occurred_at":"2026-10-05T00:00:00Z"}`,
		`{"account":"a","operation":"line\nbreak","resource":"r","result":"ok\u0001","occurred_at":"2026-10-05T00:00:00.500Z"}`,
	}
	for i, body := range bodies {
		if recorder := postEventBody(t, router, body); recorder.Code != http.StatusCreated {
			t.Fatalf("append %d: status = %d (%s)", i+1, recorder.Code, recorder.Body.String())
		}
	}

	recorder := getLedgerVerifyRequest(t, router, "/ledger/verify")
	body := decodeVerify(t, recorder)
	if !body.Valid || body.Checked != 2 || body.FirstInvalidSeq != nil {
		t.Fatalf("body = %+v, want valid/2/null for CJK/quote/backslash/control text", body)
	}
}

func TestVerifyStorageFailureReturns503WithoutPartialResult(t *testing.T) {
	st := openTestStore(t)
	router := NewRouter(st)
	if recorder := postEventBody(t, router, validEventBody()); recorder.Code != http.StatusCreated {
		t.Fatalf("seed: %s", recorder.Body.String())
	}
	st.Close()

	recorder := getLedgerVerifyRequest(t, router, "/ledger/verify")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (%s)", recorder.Code, recorder.Body.String())
	}
	var decoded struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if decoded.Error.Code != "storage_unavailable" || decoded.Error.Message == "" {
		t.Fatalf("body = %s", recorder.Body.String())
	}
	// No partial verification fields accompany the error.
	if strings.Contains(recorder.Body.String(), "checked") || strings.Contains(recorder.Body.String(), "first_invalid_seq") {
		t.Fatalf("503 body leaks partial results: %s", recorder.Body.String())
	}
	for _, leak := range []string{"SELECT", "sql", ".go", "/", "\\"} {
		if strings.Contains(recorder.Body.String(), leak) {
			t.Fatalf("error body leaks %q: %s", leak, recorder.Body.String())
		}
	}
}

func TestVerifyRacingAppendsNeverReportsFalseBreak(t *testing.T) {
	const appenders = 8
	const verifiers = 8
	router := NewRouter(openTestStore(t))

	var wg sync.WaitGroup
	results := make([]verifyBody, 0, verifiers*2)
	var mu sync.Mutex
	start := make(chan struct{})

	// Appenders commit continuously throughout the verification passes.
	for i := 0; i < appenders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			postEventBody(t, router, validEventBody())
		}()
	}
	// Each verifier may run before, during or after the appends; every pass
	// must see one complete committed chain.
	for i := 0; i < verifiers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			recorder := getLedgerVerifyRequest(t, router, "/ledger/verify")
			if recorder.Code != http.StatusOK {
				t.Errorf("verify status = %d (%s)", recorder.Code, recorder.Body.String())
				return
			}
			body := decodeVerify(t, recorder)
			mu.Lock()
			results = append(results, body)
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()

	if len(results) != verifiers {
		t.Fatalf("got %d verify results, want %d", len(results), verifiers)
	}
	for i, body := range results {
		if !body.Valid || body.FirstInvalidSeq != nil {
			t.Fatalf("verifier %d saw a mixed/invalid snapshot: %+v", i, body)
		}
		if body.Checked < 0 || body.Checked > appenders {
			t.Fatalf("verifier %d checked = %d, outside 0..%d", i, body.Checked, appenders)
		}
	}

	// After everything settles the full chain verifies with all records.
	recorder := getLedgerVerifyRequest(t, router, "/ledger/verify")
	body := decodeVerify(t, recorder)
	if !body.Valid || body.Checked != appenders || body.FirstInvalidSeq != nil {
		t.Fatalf("final body = %+v, want valid/%d/null", body, appenders)
	}
}

func TestVerifyDoesNotInterfereWithAppendChaining(t *testing.T) {
	router := NewRouter(openTestStore(t))
	first := decodeEvent(t, postEventBody(t, router, validEventBody()))

	// A verification pass between appends changes neither seq assignment nor
	// the back-link of the following record.
	if recorder := getLedgerVerifyRequest(t, router, "/ledger/verify"); recorder.Code != http.StatusOK {
		t.Fatalf("verify: %s", recorder.Body.String())
	}
	second := decodeEvent(t, postEventBody(t, router, validEventBody()))
	if second["seq"] != float64(2) || second["prev_hash"] != first["hash"] {
		t.Fatalf("second = %v, want seq 2 chained to %s", second, first["hash"])
	}
	if recorder := getLedgerVerifyRequest(t, router, "/ledger/verify"); recorder.Code != http.StatusOK {
		t.Fatalf("verify: %s", recorder.Body.String())
	}
	third := decodeEvent(t, postEventBody(t, router, validEventBody()))
	if third["seq"] != float64(3) || third["prev_hash"] != second["hash"] {
		t.Fatalf("third = %v, want seq 3 chained to the seq 2 hash", third)
	}
}

func TestVerifyUnknownPathStill404(t *testing.T) {
	router := NewRouter(openTestStore(t))
	recorder := getLedgerVerifyRequest(t, router, "/ledger")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("/ledger status = %d, want 404", recorder.Code)
	}
	recorder = getLedgerVerifyRequest(t, router, fmt.Sprintf("/ledger/verify/extra"))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("/ledger/verify/extra status = %d, want 404", recorder.Code)
	}
}

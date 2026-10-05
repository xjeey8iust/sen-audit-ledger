package api

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/xjeey8iust/sen-audit-ledger/internal/store"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "service.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func postEventBody(t *testing.T, router http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(body))
	router.ServeHTTP(recorder, request)
	return recorder
}

func validEventBody() string {
	return `{"account":"alice","operation":"login","resource":"console","result":"ok","occurred_at":"2026-10-05T08:30:00+08:00"}`
}

func decodeEvent(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return decoded
}

func wantHash(seq int64, account, operation, resource, result, occurredAt, prevHash string) string {
	// Mirror the store's escaping: backslash and double quote are escaped,
	// everything else in these test inputs stays raw.
	escape := func(s string) string {
		return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
	}
	payload := fmt.Sprintf(`[%d,"%s","%s","%s","%s","%s","%s"]`, seq,
		escape(account), escape(operation), escape(resource), escape(result), escape(occurredAt), escape(prevHash))
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

func TestPostEventAppendsGenesisRecord(t *testing.T) {
	router := NewRouter(openTestStore(t))

	recorder := postEventBody(t, router, validEventBody())
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d (%s)", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
	event := decodeEvent(t, recorder)

	if event["seq"] != float64(1) {
		t.Fatalf("seq = %v, want 1", event["seq"])
	}
	if event["prev_hash"] != strings.Repeat("0", 64) {
		t.Fatalf("prev_hash = %v, want 64 zeros", event["prev_hash"])
	}
	// occurred_at is stored and returned in UTC, fraction preserved.
	if event["occurred_at"] != "2026-10-05T00:30:00Z" {
		t.Fatalf("occurred_at = %v", event["occurred_at"])
	}
	want := wantHash(1, "alice", "login", "console", "ok", "2026-10-05T00:30:00Z", strings.Repeat("0", 64))
	if event["hash"] != want {
		t.Fatalf("hash = %v, want %v", event["hash"], want)
	}
}

func TestPostEventChainsRecords(t *testing.T) {
	router := NewRouter(openTestStore(t))

	first := decodeEvent(t, postEventBody(t, router, validEventBody()))
	second := decodeEvent(t, postEventBody(t, router,
		`{"account":"bob","operation":"read","resource":"report","result":"denied","occurred_at":"2026-10-05T01:00:00.250Z"}`))

	if second["seq"] != float64(2) {
		t.Fatalf("seq = %v, want 2", second["seq"])
	}
	if second["prev_hash"] != first["hash"] {
		t.Fatalf("prev_hash = %v, want previous hash %v", second["prev_hash"], first["hash"])
	}
	if second["occurred_at"] != "2026-10-05T01:00:00.250Z" {
		t.Fatalf("occurred_at = %v, fraction not preserved", second["occurred_at"])
	}
}

func TestPostEventKeepsOriginalFieldText(t *testing.T) {
	router := NewRouter(openTestStore(t))

	recorder := postEventBody(t, router,
		`{"account":"  alice  ","operation":"login","resource":"console","result":"ok","occurred_at":"2026-10-05T00:00:00Z"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if event := decodeEvent(t, recorder); event["account"] != "  alice  " {
		t.Fatalf("account = %q, want original text kept", event["account"])
	}
}

func TestPostEventRejectsInvalidInput(t *testing.T) {
	cases := map[string]string{
		"malformed json":      `{"account":`,
		"trailing value":      `{} {}`,
		"non object array":    `[]`,
		"non object scalar":   `42`,
		"null document":       `null`,
		"missing field":       `{"account":"a","operation":"o","resource":"r","occurred_at":"2026-10-05T00:00:00Z"}`,
		"null field":          `{"account":null,"operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z"}`,
		"wrong type":          `{"account":1,"operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z"}`,
		"blank account":       `{"account":"   ","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z"}`,
		"forbidden hash":      `{"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z","hash":"x"}`,
		"forbidden prev_hash": `{"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z","prev_hash":"x"}`,
		"unknown field":       `{"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z","note":"x"}`,
		"time no zone":        `{"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00"}`,
		"time bad date":       `{"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":"2026-02-30T00:00:00Z"}`,
		"time leap second":    `{"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":"2016-12-31T23:59:60Z"}`,
		"time bad shape":      `{"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05 00:00:00Z"}`,
		"seq fraction":        `{"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z","seq":1.5}`,
		"seq exponent":        `{"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z","seq":1e3}`,
		"seq string":          `{"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z","seq":"1"}`,
		"seq zero":            `{"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z","seq":0}`,
		"seq negative":        `{"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z","seq":-1}`,
		"seq null":            `{"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z","seq":null}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			router := NewRouter(openTestStore(t))
			recorder := postEventBody(t, router, body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d (%s)", recorder.Code, http.StatusBadRequest, recorder.Body.String())
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
			if decoded.Error.Code != "invalid_audit_input" {
				t.Fatalf("code = %q", decoded.Error.Code)
			}
			if decoded.Error.Message == "" {
				t.Fatal("message must be non-empty")
			}
		})
	}
}

func TestPostEventExplicitSeq(t *testing.T) {
	router := NewRouter(openTestStore(t))

	// Explicit seq equal to the next sequence number is accepted.
	body := `{"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z","seq":1}`
	if recorder := postEventBody(t, router, body); recorder.Code != http.StatusCreated {
		t.Fatalf("explicit next seq: status = %d (%s)", recorder.Code, recorder.Body.String())
	}

	// An existing seq conflicts.
	if recorder := postEventBody(t, router, body); recorder.Code != http.StatusConflict {
		t.Fatalf("existing seq: status = %d, want 409 (%s)", recorder.Code, recorder.Body.String())
	} else if event := decodeEvent(t, recorder); event["error"].(map[string]any)["code"] != "audit_seq_conflict" {
		t.Fatalf("existing seq: code = %v", event)
	}

	// A seq past the next one is rejected as invalid input.
	gap := `{"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z","seq":5}`
	recorder := postEventBody(t, router, gap)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("gap seq: status = %d, want 400 (%s)", recorder.Code, recorder.Body.String())
	}
	if event := decodeEvent(t, recorder); event["error"].(map[string]any)["code"] != "invalid_audit_input" {
		t.Fatalf("gap seq: body = %v", event)
	}

	// Failed appends consume no sequence number: the next append is seq 2.
	recorder = postEventBody(t, router, validEventBody())
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if event := decodeEvent(t, recorder); event["seq"] != float64(2) {
		t.Fatalf("seq = %v, want 2 after failed appends", event["seq"])
	}
}

func TestPostEventContinuesAfterReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")

	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	first := decodeEvent(t, postEventBody(t, NewRouter(st), validEventBody()))
	st.Close()

	st, err = store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st.Close()
	second := decodeEvent(t, postEventBody(t, NewRouter(st), validEventBody()))

	if second["seq"] != float64(2) || second["prev_hash"] != first["hash"] {
		t.Fatalf("after reopen: seq = %v prev_hash = %v, want 2 and %v", second["seq"], second["prev_hash"], first["hash"])
	}
}

func TestPostEventConcurrentAppendsStayGapFree(t *testing.T) {
	router := NewRouter(openTestStore(t))

	const writers = 16
	codes := make([]int, writers)
	seqs := make([]float64, writers)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			recorder := postEventBody(t, router, validEventBody())
			codes[i] = recorder.Code
			if recorder.Code == http.StatusCreated {
				seqs[i] = decodeEvent(t, recorder)["seq"].(float64)
			}
		}(i)
	}
	wg.Wait()

	seen := map[float64]bool{}
	for i := 0; i < writers; i++ {
		if codes[i] != http.StatusCreated {
			t.Fatalf("writer %d: status = %d, want 201", i, codes[i])
		}
		if seen[seqs[i]] {
			t.Fatalf("duplicate seq %v", seqs[i])
		}
		seen[seqs[i]] = true
	}
	for seq := float64(1); seq <= writers; seq++ {
		if !seen[seq] {
			t.Fatalf("seq %v missing, got %v", seq, seen)
		}
	}
}

func TestPostEventStorageFailureReturns503(t *testing.T) {
	st := openTestStore(t)
	router := NewRouter(st)
	st.Close()

	recorder := postEventBody(t, router, validEventBody())
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (%s)", recorder.Code, recorder.Body.String())
	}
	if event := decodeEvent(t, recorder); event["error"].(map[string]any)["code"] != "storage_unavailable" {
		t.Fatalf("body = %v", event)
	}
}

// storedEvent is one row read back from the events table.
type storedEvent struct {
	seq        int64
	account    string
	occurredAt string
	prevHash   string
	hash       string
}

// readStoredEvents opens the database file directly and returns every
// persisted record in seq order, so tests verify what was actually saved.
func readStoredEvents(t *testing.T, path string) []storedEvent {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT seq, account, occurred_at, prev_hash, hash FROM events ORDER BY seq")
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	defer rows.Close()
	var events []storedEvent
	for rows.Next() {
		var e storedEvent
		if err := rows.Scan(&e.seq, &e.account, &e.occurredAt, &e.prevHash, &e.hash); err != nil {
			t.Fatalf("scan event: %v", err)
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate events: %v", err)
	}
	return events
}

// wantInvalidInput posts one event body and asserts the invalid_audit_input
// rejection shape: 400, only a top-level error object, and no internals.
func wantInvalidInput(t *testing.T, router http.Handler, body string) {
	t.Helper()
	recorder := postEventBody(t, router, body)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d (%s)", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
	decoded := decodeEvent(t, recorder)
	if len(decoded) != 1 {
		t.Fatalf("response keys = %v, want only the error object", decoded)
	}
	errObj, ok := decoded["error"].(map[string]any)
	if !ok {
		t.Fatalf("error = %v, want an object", decoded["error"])
	}
	if errObj["code"] != "invalid_audit_input" {
		t.Fatalf("code = %v, want invalid_audit_input", errObj["code"])
	}
	if message, ok := errObj["message"].(string); !ok || message == "" {
		t.Fatalf("message = %v, want a non-empty string", errObj["message"])
	}
	leaks := []string{"sql", "SELECT", "INSERT", "goroutine", ".go", "/", "\\"}
	for _, leak := range leaks {
		if strings.Contains(recorder.Body.String(), leak) {
			t.Fatalf("response leaks internals (%q): %s", leak, recorder.Body.String())
		}
	}
}

func TestPostEventRejectsUTCYearOutOfRange(t *testing.T) {
	cases := map[string]string{
		"below year 0000":          `{"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":"0000-01-01T00:00:00+00:01"}`,
		"below year 0000 fraction": `{"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":"0000-01-01T00:00:00.5+00:01"}`,
		"above year 9999":          `{"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":"9999-12-31T23:59:59-00:01"}`,
		"above year 9999 fraction": `{"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":"9999-12-31T23:59:59.5-00:01"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			router := NewRouter(openTestStore(t))
			wantInvalidInput(t, router, body)
		})
	}
}

func TestPostEventRejectsOutOfRangeTimeBeforeSeqChecks(t *testing.T) {
	router := NewRouter(openTestStore(t))

	// One valid record so seq 1 exists and seq 9 is a gap.
	if recorder := postEventBody(t, router, validEventBody()); recorder.Code != http.StatusCreated {
		t.Fatalf("seed: status = %d (%s)", recorder.Code, recorder.Body.String())
	}

	// An out-of-range occurred_at is invalid input no matter what seq says.
	existing := `{"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":"0000-01-01T00:00:00+00:01","seq":1}`
	wantInvalidInput(t, router, existing)
	gap := `{"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":"9999-12-31T23:59:59-00:01","seq":9}`
	wantInvalidInput(t, router, gap)
}

func TestPostEventAcceptsBoundaryUTCYear(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	router := NewRouter(st)

	cases := []struct {
		occurredAt string
		wantUTC    string
	}{
		// One minute inside the lower bound: UTC lands exactly on 0000-01-01T00:00:00Z.
		{"0000-01-01T00:01:00+00:01", "0000-01-01T00:00:00Z"},
		// One minute inside the upper bound: UTC lands exactly on 9999-12-31T23:59:59Z.
		{"9999-12-31T23:58:59-00:01", "9999-12-31T23:59:59Z"},
	}
	prevHash := strings.Repeat("0", 64)
	for i, tc := range cases {
		recorder := postEventBody(t, router,
			fmt.Sprintf(`{"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":%q}`, tc.occurredAt))
		if recorder.Code != http.StatusCreated {
			t.Fatalf("%s: status = %d (%s)", tc.occurredAt, recorder.Code, recorder.Body.String())
		}
		event := decodeEvent(t, recorder)
		if event["occurred_at"] != tc.wantUTC {
			t.Fatalf("%s: occurred_at = %v, want %v", tc.occurredAt, event["occurred_at"], tc.wantUTC)
		}
		seq := int64(i + 1)
		if event["seq"] != float64(seq) {
			t.Fatalf("%s: seq = %v, want %d", tc.occurredAt, event["seq"], seq)
		}
		want := wantHash(seq, "a", "o", "r", "ok", tc.wantUTC, prevHash)
		if event["hash"] != want {
			t.Fatalf("%s: hash = %v, want %v (normalized time must feed the hash)", tc.occurredAt, event["hash"], want)
		}
		prevHash = event["hash"].(string)
	}

	// The persisted rows carry the same normalized UTC timestamps.
	stored := readStoredEvents(t, path)
	if len(stored) != len(cases) {
		t.Fatalf("stored %d events, want %d", len(stored), len(cases))
	}
	for i, tc := range cases {
		if stored[i].occurredAt != tc.wantUTC {
			t.Fatalf("stored occurred_at = %v, want %v", stored[i].occurredAt, tc.wantUTC)
		}
	}
}

func TestPostEventOffsetConversionAcrossBoundaries(t *testing.T) {
	router := NewRouter(openTestStore(t))

	cases := []struct {
		occurredAt string
		wantUTC    string
	}{
		{"2026-10-05T00:30:00+08:00", "2026-10-04T16:30:00Z"},           // cross day
		{"2026-03-01T00:15:00+05:30", "2026-02-28T18:45:00Z"},           // cross month
		{"2026-01-01T00:30:00+08:00", "2025-12-31T16:30:00Z"},           // cross year
		{"2026-10-05T01:00:00.2500+08:00", "2026-10-04T17:00:00.2500Z"}, // fraction digits and trailing zeros kept
		{"2026-10-05T08:30:00Z", "2026-10-05T08:30:00Z"},                // Z input unchanged
		{"2026-10-04T23:00:00-02:00", "2026-10-05T01:00:00Z"},           // negative offset forward
	}
	for _, tc := range cases {
		recorder := postEventBody(t, router,
			fmt.Sprintf(`{"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":%q}`, tc.occurredAt))
		if recorder.Code != http.StatusCreated {
			t.Fatalf("%s: status = %d (%s)", tc.occurredAt, recorder.Code, recorder.Body.String())
		}
		if event := decodeEvent(t, recorder); event["occurred_at"] != tc.wantUTC {
			t.Fatalf("%s: occurred_at = %v, want %v", tc.occurredAt, event["occurred_at"], tc.wantUTC)
		}
	}
}

func TestPostEventRejectedTimeLeavesChainUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")

	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	router := NewRouter(st)

	// Out-of-range times, with and without seq, are all rejected.
	wantInvalidInput(t, router, `{"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":"0000-01-01T00:00:00+00:01"}`)
	wantInvalidInput(t, router, `{"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":"9999-12-31T23:59:59-00:01","seq":1}`)
	wantInvalidInput(t, router, `{"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":"9999-12-31T23:59:59-00:01","seq":9}`)

	// The rejections consumed no sequence number and moved no chain tail.
	recorder := postEventBody(t, router, validEventBody())
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	first := decodeEvent(t, recorder)
	if first["seq"] != float64(1) || first["prev_hash"] != strings.Repeat("0", 64) {
		t.Fatalf("first = %v, want seq 1 chained to genesis", first)
	}
	st.Close()

	// Only the valid append was persisted.
	stored := readStoredEvents(t, path)
	if len(stored) != 1 {
		t.Fatalf("stored %d events, want 1", len(stored))
	}
	if stored[0].seq != 1 || stored[0].occurredAt != "2026-10-05T00:30:00Z" || stored[0].hash != first["hash"] {
		t.Fatalf("stored event = %+v, want the valid append", stored[0])
	}

	// After a close and reopen the chain continues from the same tail.
	st, err = store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st.Close()
	second := decodeEvent(t, postEventBody(t, NewRouter(st), validEventBody()))
	if second["seq"] != float64(2) || second["prev_hash"] != first["hash"] {
		t.Fatalf("after reopen: seq = %v prev_hash = %v, want 2 and %v", second["seq"], second["prev_hash"], first["hash"])
	}
	stored = readStoredEvents(t, path)
	if len(stored) != 2 || stored[1].hash != second["hash"] {
		t.Fatalf("stored events after reopen = %+v", stored)
	}
}

func TestPostEventRejectsInvalidUTF8(t *testing.T) {
	cases := map[string]string{
		"invalid byte in value":     `{"account":"a` + "\xff" + `","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z"}`,
		"truncated multibyte value": `{"account":"` + "\xe4\xb8" + `","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z"}`,
		"invalid byte in key":       `{"acc` + "\xff" + `ount":"a","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z"}`,
		"overlong encoding":         `{"account":"` + "\xc0\xaf" + `","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z"}`,
		"utf8 encoded surrogate":    `{"account":"` + "\xed\xa0\x80" + `","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			router := NewRouter(openTestStore(t))
			wantInvalidInput(t, router, body)
		})
	}
}

func TestPostEventRejectsUnpairedSurrogates(t *testing.T) {
	cases := map[string]string{
		"lone high surrogate":       `{"account":"\uD83D","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z"}`,
		"lone low surrogate":        `{"account":"\uDE00","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z"}`,
		"reversed pair":             `{"account":"\uDE00\uD83D","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z"}`,
		"high then non surrogate":   `{"account":"\uD83DA","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z"}`,
		"high at end of string":     `{"account":"a\uD83D","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z"}`,
		"surrogates across strings": `{"account":"\uD83D","operation":"\uDE00","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z"}`,
		"lowercase hex":             `{"account":"\ud83d","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z"}`,
		"surrogate in occurred_at":  `{"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z\uD83D"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			router := NewRouter(openTestStore(t))
			wantInvalidInput(t, router, body)
		})
	}
}

func TestPostEventAcceptsValidUnicode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	router := NewRouter(st)

	cases := []struct {
		name        string
		accountJSON string // JSON literal used for the account field
		wantAccount string // decoded text the ledger must store verbatim
	}{
		{"direct chinese", `"账号"`, "账号"},
		{"escaped chinese", `"\u8d26\u53f7"`, "账号"},
		{"direct supplementary plane", `"😀"`, "😀"},
		{"escaped supplementary plane", `"\uD83D\uDE00"`, "😀"},
		{"escaped supplementary plane lowercase", `"\ud83d\ude00"`, "😀"},
		{"direct replacement char", `"�"`, "�"},
		{"escaped replacement char", `"\ufffd"`, "�"},
		{"escaped backslash text", `"\\uD83D"`, `\uD83D`},
	}
	prevHash := strings.Repeat("0", 64)
	for i, tc := range cases {
		body := `{"account":` + tc.accountJSON + `,"operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z"}`
		recorder := postEventBody(t, router, body)
		if recorder.Code != http.StatusCreated {
			t.Fatalf("%s: status = %d (%s)", tc.name, recorder.Code, recorder.Body.String())
		}
		event := decodeEvent(t, recorder)
		if event["account"] != tc.wantAccount {
			t.Fatalf("%s: account = %q, want %q", tc.name, event["account"], tc.wantAccount)
		}
		// The hash is computed over the decoded text, so an escaped
		// surrogate pair and the same character submitted directly
		// produce identical hashes at identical chain positions.
		seq := int64(i + 1)
		want := wantHash(seq, tc.wantAccount, "o", "r", "ok", "2026-10-05T00:00:00Z", prevHash)
		if event["hash"] != want {
			t.Fatalf("%s: hash = %v, want %v", tc.name, event["hash"], want)
		}
		prevHash = event["hash"].(string)
	}

	// What was persisted matches the decoded text exactly.
	stored := readStoredEvents(t, path)
	if len(stored) != len(cases) {
		t.Fatalf("stored %d events, want %d", len(stored), len(cases))
	}
	for i, tc := range cases {
		if stored[i].account != tc.wantAccount {
			t.Fatalf("%s: stored account = %q, want %q", tc.name, stored[i].account, tc.wantAccount)
		}
	}
}

func TestPostEventRejectedEncodingLeavesChainUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	router := NewRouter(st)

	// One valid record so seq 1 exists and seq 9 is a gap.
	seed := postEventBody(t, router, validEventBody())
	if seed.Code != http.StatusCreated {
		t.Fatalf("seed: status = %d (%s)", seed.Code, seed.Body.String())
	}
	first := decodeEvent(t, seed)

	// Invalid UTF-8 bytes and unpaired surrogates are rejected no matter
	// what seq the request carries: omitted, already existing, or a gap.
	accounts := []string{
		`"a` + "\xff" + `"`,    // invalid UTF-8 byte
		`"` + "\xe4\xb8" + `"`, // truncated multibyte character
		`"\uD83D"`,             // lone high surrogate
		`"\uDE00"`,             // lone low surrogate
		`"\uDE00\uD83D"`,       // reversed pair
	}
	for _, account := range accounts {
		for _, seq := range []string{"", `,"seq":1`, `,"seq":9`} {
			body := `{"account":` + account +
				`,"operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z"` + seq + `}`
			wantInvalidInput(t, router, body)
		}
	}

	// The rejections consumed no sequence number and moved no chain tail.
	stored := readStoredEvents(t, path)
	if len(stored) != 1 || stored[0].hash != first["hash"] {
		t.Fatalf("stored events = %+v, want only the seed record", stored)
	}

	// The next valid append continues from the original chain tail.
	recorder := postEventBody(t, router, validEventBody())
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	second := decodeEvent(t, recorder)
	if second["seq"] != float64(2) || second["prev_hash"] != first["hash"] {
		t.Fatalf("next = %v, want seq 2 chained to %v", second, first["hash"])
	}
	stored = readStoredEvents(t, path)
	if len(stored) != 2 || stored[1].hash != second["hash"] {
		t.Fatalf("stored events after valid append = %+v", stored)
	}
}

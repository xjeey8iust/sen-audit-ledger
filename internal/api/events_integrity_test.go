package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/xjeey8iust/sen-audit-ledger/internal/store"
)

// integrityEvent describes one request body and the record it must produce.
// Seq is nil when the request lets the service assign the sequence number.
type integrityEvent struct {
	account    string
	operation  string
	resource   string
	result     string
	occurredAt string // occurred_at sent in the request
	wantUTC    string // UTC form the service must store and return
	seq        *int64
}

func (e integrityEvent) body() string {
	fields := map[string]any{
		"account":     e.account,
		"operation":   e.operation,
		"resource":    e.resource,
		"result":      e.result,
		"occurred_at": e.occurredAt,
	}
	if e.seq != nil {
		fields["seq"] = *e.seq
	}
	body, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	return string(body)
}

// integrityResponse is the decoded 201 body the tests chain and persist.
type integrityResponse struct {
	Account    string `json:"account"`
	Operation  string `json:"operation"`
	Resource   string `json:"resource"`
	Result     string `json:"result"`
	OccurredAt string `json:"occurred_at"`
	Seq        int64  `json:"seq"`
	PrevHash   string `json:"prev_hash"`
	Hash       string `json:"hash"`
}

// integrityRow is one full row read back from the events table.
type integrityRow struct {
	seq        int64
	account    string
	operation  string
	resource   string
	result     string
	occurredAt string
	prevHash   string
	hash       string
}

// integrityHash recomputes a record hash from the public rule in README.md:
// SHA-256 (lowercase hex) over the UTF-8 bytes of the compact JSON array
// [seq, account, operation, resource, result, occurred_at, prev_hash], where
// strings escape only the double quote, the backslash and control characters
// (as lowercase xx). It is a test-local implementation so the
// expected values never come from the product's own hash function.
func integrityHash(seq int64, account, operation, resource, result, occurredAt, prevHash string) string {
	const hexdigits = "0123456789abcdef"
	var b strings.Builder
	b.WriteByte('[')
	b.WriteString(strconv.FormatInt(seq, 10))
	for _, field := range []string{account, operation, resource, result, occurredAt, prevHash} {
		b.WriteByte(',')
		b.WriteByte('"')
		for i := 0; i < len(field); i++ {
			c := field[i]
			switch {
			case c == '"' || c == '\\':
				b.WriteByte('\\')
				b.WriteByte(c)
			case c < 0x20:
				b.WriteString(`\u00`)
				b.WriteByte(hexdigits[c>>4])
				b.WriteByte(hexdigits[c&0x0f])
			default:
				b.WriteByte(c)
			}
		}
		b.WriteByte('"')
	}
	b.WriteByte(']')
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// readIntegrityRows opens the database file directly and returns every
// persisted record with all columns, in seq order.
func readIntegrityRows(t *testing.T, path string) []integrityRow {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT seq, account, operation, resource, result, occurred_at, prev_hash, hash FROM events ORDER BY seq")
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	defer rows.Close()
	var events []integrityRow
	for rows.Next() {
		var e integrityRow
		if err := rows.Scan(&e.seq, &e.account, &e.operation, &e.resource, &e.result, &e.occurredAt, &e.prevHash, &e.hash); err != nil {
			t.Fatalf("scan event: %v", err)
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate events: %v", err)
	}
	return events
}

// reopenStore closes the store and reopens the same database file, so the
// checks that follow prove the ledger survives a restart.
func reopenStore(t *testing.T, st *store.Store, path string) {
	t.Helper()
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { reopened.Close() })
}

// postIntegrity posts one event that must succeed and decodes the 201 body.
func postIntegrity(t *testing.T, router http.Handler, ev integrityEvent) integrityResponse {
	t.Helper()
	recorder := postEventBody(t, router, ev.body())
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d (%s)", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
	var resp integrityResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp
}

// wantResponse checks a 201 body against the request that produced it: the
// business fields keep their original text, occurred_at is the expected UTC
// form, and seq, prev_hash and hash follow the chain rules.
func wantResponse(t *testing.T, resp integrityResponse, ev integrityEvent, seq int64, prevHash string) {
	t.Helper()
	if resp.Account != ev.account || resp.Operation != ev.operation ||
		resp.Resource != ev.resource || resp.Result != ev.result {
		t.Fatalf("response fields = %+v, want the request fields %+v", resp, ev)
	}
	if resp.OccurredAt != ev.wantUTC {
		t.Fatalf("occurred_at = %q, want UTC %q", resp.OccurredAt, ev.wantUTC)
	}
	if resp.Seq != seq {
		t.Fatalf("seq = %d, want %d", resp.Seq, seq)
	}
	if resp.PrevHash != prevHash {
		t.Fatalf("prev_hash = %q, want %q", resp.PrevHash, prevHash)
	}
	if want := integrityHash(seq, ev.account, ev.operation, ev.resource, ev.result, ev.wantUTC, prevHash); resp.Hash != want {
		t.Fatalf("hash = %q, want %q", resp.Hash, want)
	}
}

// wantStoredChain verifies the persisted rows against the successful
// responses in seq order: exactly one row per success, every field matching
// the response, an unbroken prev_hash chain from genesis, and every hash
// recomputed from the public rule.
func wantStoredChain(t *testing.T, rows []integrityRow, responses map[int64]integrityResponse) {
	t.Helper()
	if len(rows) != len(responses) {
		t.Fatalf("stored %d events, want %d", len(rows), len(responses))
	}
	prevHash := strings.Repeat("0", 64)
	for i, row := range rows {
		seq := int64(i + 1)
		resp, ok := responses[seq]
		if !ok {
			t.Fatalf("no successful response for stored seq %d", seq)
		}
		if row.seq != seq {
			t.Fatalf("row %d: seq = %d, want %d (seqs must start at 1 and be gap-free)", i, row.seq, seq)
		}
		if row.account != resp.Account || row.operation != resp.Operation ||
			row.resource != resp.Resource || row.result != resp.Result ||
			row.occurredAt != resp.OccurredAt {
			t.Fatalf("row %d = %+v, want the fields from the 201 response %+v", seq, row, resp)
		}
		if row.prevHash != prevHash || resp.PrevHash != prevHash {
			t.Fatalf("row %d: prev_hash = %q (response %q), want %q", seq, row.prevHash, resp.PrevHash, prevHash)
		}
		want := integrityHash(row.seq, row.account, row.operation, row.resource, row.result, row.occurredAt, row.prevHash)
		if row.hash != want {
			t.Fatalf("row %d: stored hash = %q, recomputed %q", seq, row.hash, want)
		}
		if resp.Hash != want {
			t.Fatalf("row %d: response hash = %q, recomputed %q", seq, resp.Hash, want)
		}
		prevHash = row.hash
	}
}

// wantCleanError asserts the published error shape: the expected status, a
// single top-level error object whose code and message are strings, and a
// message that leaks no SQL, stack trace or file path.
func wantCleanError(t *testing.T, recorder *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	if recorder.Code != wantStatus {
		t.Fatalf("status = %d, want %d (%s)", recorder.Code, wantStatus, recorder.Body.String())
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if len(decoded) != 1 {
		t.Fatalf("response keys = %v, want only the error object", decoded)
	}
	raw, ok := decoded["error"]
	if !ok {
		t.Fatalf("missing top-level error object: %s", recorder.Body.String())
	}
	var errObj struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &errObj); err != nil {
		t.Fatalf("error is not an object with string code and message: %v", err)
	}
	if errObj.Code != wantCode {
		t.Fatalf("code = %q, want %q", errObj.Code, wantCode)
	}
	if errObj.Message == "" {
		t.Fatal("message must be a non-empty string")
	}
	leaks := []string{"sql", "SELECT", "INSERT", "goroutine", ".go", "/", "\\"}
	for _, leak := range leaks {
		if strings.Contains(recorder.Body.String(), leak) {
			t.Fatalf("response leaks internals (%q): %s", leak, recorder.Body.String())
		}
	}
}

// postConcurrent fires one request per event at the same instant on one
// service instance and returns each request's recorder by event index.
func postConcurrent(t *testing.T, router http.Handler, events []integrityEvent) []*httptest.ResponseRecorder {
	t.Helper()
	recorders := make([]*httptest.ResponseRecorder, len(events))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, ev := range events {
		wg.Add(1)
		go func(i int, ev integrityEvent) {
			defer wg.Done()
			<-start
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(ev.body()))
			router.ServeHTTP(recorder, request)
			recorders[i] = recorder
		}(i, ev)
	}
	close(start)
	wg.Wait()
	return recorders
}

func TestPostEventConcurrentDistinctAppendsPersistExactlyOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	router := NewRouter(st)

	// Distinguishable contents: quotes and backslashes, non-ASCII text, a
	// control character, and timestamps that cross day, month and year
	// boundaries when converted to UTC, fractions preserved.
	events := []integrityEvent{
		{account: "alice", operation: "login", resource: "console", result: "ok", occurredAt: "2026-10-05T08:30:00+08:00", wantUTC: "2026-10-05T00:30:00Z"},
		{account: `quote "and" \slash`, operation: "export", resource: "report", result: "denied", occurredAt: "2026-10-05T01:00:00.250Z", wantUTC: "2026-10-05T01:00:00.250Z"},
		{account: "账号😀", operation: "读取", resource: "仪表盘", result: "ok", occurredAt: "2026-03-01T00:15:00+05:30", wantUTC: "2026-02-28T18:45:00Z"},
		{account: "tab\there", operation: "op-4", resource: "res-4", result: "ok", occurredAt: "2026-10-04T23:00:00-02:00", wantUTC: "2026-10-05T01:00:00Z"},
		{account: "user-5", operation: "op-5", resource: "res-5", result: "fail", occurredAt: "2026-01-01T00:30:00+08:00", wantUTC: "2025-12-31T16:30:00Z"},
		{account: "user-6", operation: "op-6", resource: "res-6", result: "ok", occurredAt: "2026-10-05T12:00:00.1234567Z", wantUTC: "2026-10-05T12:00:00.1234567Z"},
		{account: "user-7", operation: "op-7", resource: "res-7", result: "ok", occurredAt: "2026-06-15T18:45:00.5+02:00", wantUTC: "2026-06-15T16:45:00.5Z"},
		{account: "user-8", operation: "op-8", resource: "res-8", result: "ok", occurredAt: "2026-12-31T23:59:59-01:00", wantUTC: "2027-01-01T00:59:59Z"},
	}
	byAccount := map[string]integrityEvent{}
	for _, ev := range events {
		byAccount[ev.account] = ev
	}

	recorders := postConcurrent(t, router, events)

	// Every request succeeds; the seq in each response — not the submission
	// or completion order — says where the record landed in the chain.
	responses := map[int64]integrityResponse{}
	for i, recorder := range recorders {
		if recorder.Code != http.StatusCreated {
			t.Fatalf("request %d: status = %d, want 201 (%s)", i, recorder.Code, recorder.Body.String())
		}
		var resp integrityResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
			t.Fatalf("request %d: decode response: %v", i, err)
		}
		ev, ok := byAccount[resp.Account]
		if !ok {
			t.Fatalf("request %d: response account %q matches no submitted event", i, resp.Account)
		}
		if resp.Operation != ev.operation || resp.Resource != ev.resource || resp.Result != ev.result {
			t.Fatalf("request %d: response fields = %+v, want the request fields %+v", i, resp, ev)
		}
		if resp.OccurredAt != ev.wantUTC {
			t.Fatalf("request %d: occurred_at = %q, want UTC %q", i, resp.OccurredAt, ev.wantUTC)
		}
		if _, dup := responses[resp.Seq]; dup {
			t.Fatalf("request %d: duplicate seq %d", i, resp.Seq)
		}
		responses[resp.Seq] = resp
	}

	// The assigned seqs are exactly 1..N and the chain recomputes end to end.
	for seq := int64(1); seq <= int64(len(events)); seq++ {
		if _, ok := responses[seq]; !ok {
			t.Fatalf("seq %d missing from responses", seq)
		}
	}
	prevHash := strings.Repeat("0", 64)
	for seq := int64(1); seq <= int64(len(events)); seq++ {
		resp := responses[seq]
		if resp.PrevHash != prevHash {
			t.Fatalf("seq %d: prev_hash = %q, want %q", seq, resp.PrevHash, prevHash)
		}
		want := integrityHash(seq, resp.Account, resp.Operation, resp.Resource, resp.Result, resp.OccurredAt, resp.PrevHash)
		if resp.Hash != want {
			t.Fatalf("seq %d: hash = %q, want %q", seq, resp.Hash, want)
		}
		prevHash = resp.Hash
	}

	// After a close and reopen, every request was saved exactly once, with
	// the fields and chain the responses reported.
	reopenStore(t, st, path)
	wantStoredChain(t, readIntegrityRows(t, path), responses)
}

func TestPostEventConcurrentSameSeqExactlyOneSucceeds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	router := NewRouter(st)

	seed := integrityEvent{account: "seed", operation: "boot", resource: "system", result: "ok", occurredAt: "2026-10-05T00:00:00Z", wantUTC: "2026-10-05T00:00:00Z"}
	seedResp := postIntegrity(t, router, seed)
	wantResponse(t, seedResp, seed, 1, strings.Repeat("0", 64))

	// Distinct contents race for the same legal next sequence number.
	two := int64(2)
	racers := []integrityEvent{
		{account: "racer-1", operation: "write", resource: "ledger", result: "ok", occurredAt: "2026-10-05T01:00:00Z", wantUTC: "2026-10-05T01:00:00Z", seq: &two},
		{account: "racer-2", operation: "write", resource: "ledger", result: "ok", occurredAt: "2026-10-05T01:00:01Z", wantUTC: "2026-10-05T01:00:01Z", seq: &two},
		{account: "racer-3", operation: "write", resource: "ledger", result: "ok", occurredAt: "2026-10-05T01:00:02Z", wantUTC: "2026-10-05T01:00:02Z", seq: &two},
		{account: "racer-4", operation: "write", resource: "ledger", result: "ok", occurredAt: "2026-10-05T01:00:03Z", wantUTC: "2026-10-05T01:00:03Z", seq: &two},
		{account: "racer-5", operation: "write", resource: "ledger", result: "ok", occurredAt: "2026-10-05T01:00:04Z", wantUTC: "2026-10-05T01:00:04Z", seq: &two},
		{account: "racer-6", operation: "write", resource: "ledger", result: "ok", occurredAt: "2026-10-05T01:00:05Z", wantUTC: "2026-10-05T01:00:05Z", seq: &two},
	}

	recorders := postConcurrent(t, router, racers)

	// Exactly one request wins; the rest get the published conflict shape.
	var winnerResp integrityResponse
	var winnerEvent integrityEvent
	wins := 0
	for i, recorder := range recorders {
		if recorder.Code == http.StatusCreated {
			wins++
			if err := json.Unmarshal(recorder.Body.Bytes(), &winnerResp); err != nil {
				t.Fatalf("racer %d: decode response: %v", i, err)
			}
			winnerEvent = racers[i]
			continue
		}
		wantCleanError(t, recorder, http.StatusConflict, "audit_seq_conflict")
	}
	if wins != 1 {
		t.Fatalf("%d requests returned 201, want exactly 1", wins)
	}

	// The stored record belongs to the unique successful request.
	wantResponse(t, winnerResp, winnerEvent, 2, seedResp.Hash)

	// After the race, a seq-omitted append takes the very next seq.
	follow := integrityEvent{account: "follow-up", operation: "read", resource: "ledger", result: "ok", occurredAt: "2026-10-05T02:00:00Z", wantUTC: "2026-10-05T02:00:00Z"}
	followResp := postIntegrity(t, router, follow)
	wantResponse(t, followResp, follow, 3, winnerResp.Hash)

	// After a close and reopen the chain holds and the losing requests left
	// no records behind.
	reopenStore(t, st, path)
	wantStoredChain(t, readIntegrityRows(t, path), map[int64]integrityResponse{
		1: seedResp,
		2: winnerResp,
		3: followResp,
	})
}

func TestPostEventWriteFailureWhileDatabaseReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	router := NewRouter(st)

	first := integrityEvent{account: "first", operation: "login", resource: "console", result: "ok", occurredAt: "2026-10-05T08:30:00+08:00", wantUTC: "2026-10-05T00:30:00Z"}
	firstResp := postIntegrity(t, router, first)
	wantResponse(t, firstResp, first, 1, strings.Repeat("0", 64))
	second := integrityEvent{account: "second", operation: "read", resource: "report", result: "ok", occurredAt: "2026-10-05T01:00:00.5Z", wantUTC: "2026-10-05T01:00:00.5Z"}
	secondResp := postIntegrity(t, router, second)
	wantResponse(t, secondResp, second, 2, firstResp.Hash)

	// Hold the write lock on a second connection: the database stays
	// readable, but no write can commit until the lock is released.
	lockDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open lock connection: %v", err)
	}
	lockDB.SetMaxOpenConns(1)
	ctx := context.Background()
	conn, err := lockDB.Conn(ctx)
	if err != nil {
		t.Fatalf("pin lock connection: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
		t.Fatalf("begin exclusive: %v", err)
	}

	// Reads keep working while writes are blocked: healthz stays green.
	health := httptest.NewRecorder()
	router.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK || health.Body.String() != `{"database":"ok","status":"ok"}` {
		t.Fatalf("healthz during write lock: status = %d, body = %s", health.Code, health.Body.String())
	}

	// One controlled write failure: the append cannot commit.
	outage := integrityEvent{account: "during-outage", operation: "write", resource: "ledger", result: "ok", occurredAt: "2026-10-05T03:00:00Z", wantUTC: "2026-10-05T03:00:00Z"}
	wantCleanError(t, postEventBody(t, router, outage.body()), http.StatusServiceUnavailable, "storage_unavailable")

	// Existing records, their count and the chain tail are untouched.
	wantStoredChain(t, readIntegrityRows(t, path), map[int64]integrityResponse{
		1: firstResp,
		2: secondResp,
	})

	// Restore writes.
	if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close lock connection: %v", err)
	}
	if err := lockDB.Close(); err != nil {
		t.Fatalf("close lock database: %v", err)
	}

	// A legal append reuses the original next seq and chain tail hash.
	third := integrityEvent{account: "third", operation: "write", resource: "ledger", result: "ok", occurredAt: "2026-10-05T04:00:00Z", wantUTC: "2026-10-05T04:00:00Z"}
	thirdResp := postIntegrity(t, router, third)
	wantResponse(t, thirdResp, third, 3, secondResp.Hash)

	// Input validation and error mapping are unchanged after the outage.
	wantInvalidInput(t, router, `{"account":"x"}`)

	// After a close and reopen the chain is intact and the failed request
	// left no record behind.
	reopenStore(t, st, path)
	rows := readIntegrityRows(t, path)
	wantStoredChain(t, rows, map[int64]integrityResponse{
		1: firstResp,
		2: secondResp,
		3: thirdResp,
	})
	for _, row := range rows {
		if row.account == outage.account {
			t.Fatalf("failed request was persisted: %+v", row)
		}
	}
}

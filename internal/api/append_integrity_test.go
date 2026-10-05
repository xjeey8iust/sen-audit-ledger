package api

// Regression tests for append transaction integrity:
//
//   - concurrent auto-sequenced appends on one service instance must all
//     return 201, save each request exactly once with a contiguous chain,
//     and keep every business field verbatim;
//   - concurrent requests racing for one explicit next seq must elect
//     exactly one winner (201) while the others get 409 audit_seq_conflict;
//   - a controlled write failure while the database stays readable must
//     surface as 503 storage_unavailable, consume no sequence number and
//     leave the chain tail untouched until writes recover.
//
// Every scenario cross-checks the 201 response against the rows actually
// persisted in a temporary SQLite database (read through a separate
// connection), recomputes each hash from the public SHA-256 rule in
// README.md rather than calling product internals, and repeats the full
// verification after the database is closed and reopened.

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/xjeey8iust/sen-audit-ledger/internal/store"
)

// createdEvent is the typed 201 response body.
type createdEvent struct {
	Account    string `json:"account"`
	Operation  string `json:"operation"`
	Resource   string `json:"resource"`
	Result     string `json:"result"`
	OccurredAt string `json:"occurred_at"`
	Seq        int64  `json:"seq"`
	PrevHash   string `json:"prev_hash"`
	Hash       string `json:"hash"`
}

// eventRequest builds one POST /events body. Seq is omitted when nil.
type eventRequest struct {
	Account    string `json:"account"`
	Operation  string `json:"operation"`
	Resource   string `json:"resource"`
	Result     string `json:"result"`
	OccurredAt string `json:"occurred_at"`
	Seq        *int64 `json:"seq,omitempty"`
}

// eventFixture pairs a request with the UTC form the service must store.
type eventFixture struct {
	req         eventRequest
	occurredUTC string
}

// ledgerRecord is the business-field expectation for one persisted row.
type ledgerRecord struct {
	account    string
	operation  string
	resource   string
	result     string
	occurredAt string
}

// persistedEvent is one row read back through an independent connection.
type persistedEvent struct {
	seq        int64
	account    string
	operation  string
	resource   string
	result     string
	occurredAt string
	prevHash   string
	hash       string
}

func openStoreAt(t *testing.T, path string) *store.Store {
	t.Helper()
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store %s: %v", path, err)
	}
	return st
}

func closeStore(t *testing.T, st *store.Store) {
	t.Helper()
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
}

func mustEventJSON(t *testing.T, e eventRequest) string {
	t.Helper()
	var b strings.Builder
	encoder := json.NewEncoder(&b)
	// Keep non-ASCII and angle brackets raw in the request; the server
	// decodes either way and hashes the decoded text.
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(e); err != nil {
		t.Fatalf("encode event: %v", err)
	}
	return b.String()
}

func postStructuredEvent(t *testing.T, router http.Handler, e eventRequest) *httptest.ResponseRecorder {
	t.Helper()
	return postEventBody(t, router, mustEventJSON(t, e))
}

func decodeCreated(t *testing.T, recorder *httptest.ResponseRecorder) createdEvent {
	t.Helper()
	var got createdEvent
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode 201 body: %v (%s)", err, recorder.Body.String())
	}
	return got
}

// assertErrorEnvelope checks the published error contract for 409/503
// responses: exactly one top-level error object whose code and message are
// non-empty strings, with no SQL, stack frame or file path in the body.
func assertErrorEnvelope(t *testing.T, recorder *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status = %d, want %d (%s)", recorder.Code, status, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v (%s)", err, recorder.Body.String())
	}
	if len(body) != 1 {
		t.Fatalf("top-level keys = %v, want only the error object", body)
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("error = %v, want an object", body["error"])
	}
	if len(errObj) != 2 {
		t.Fatalf("error keys = %v, want only code and message", errObj)
	}
	if errObj["code"] != code {
		t.Fatalf("code = %v, want %s", errObj["code"], code)
	}
	message, ok := errObj["message"].(string)
	if !ok || message == "" {
		t.Fatalf("message = %v, want a non-empty string", errObj["message"])
	}
	for _, leak := range []string{"sql", "SQL", "SELECT", "INSERT", "UPDATE", "goroutine", ".go", "/", "\\"} {
		if strings.Contains(recorder.Body.String(), leak) {
			t.Fatalf("error response leaks %q: %s", leak, recorder.Body.String())
		}
	}
}

// readFullLedger opens the database file independently of the service and
// returns every persisted record in seq order.
func readFullLedger(t *testing.T, path string) []persistedEvent {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT seq, account, operation, resource, result, occurred_at, prev_hash, hash
		FROM events ORDER BY seq`)
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	defer rows.Close()
	var events []persistedEvent
	for rows.Next() {
		var e persistedEvent
		if err := rows.Scan(&e.seq, &e.account, &e.operation, &e.resource, &e.result,
			&e.occurredAt, &e.prevHash, &e.hash); err != nil {
			t.Fatalf("scan event: %v", err)
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate events: %v", err)
	}
	return events
}

// verifyLedger compares the whole persisted chain with the successful
// responses: exact row count, contiguous seqs from 1, verbatim business
// fields, genesis/previous-hash linkage, and hashes recomputed from the
// public rule. responseHash additionally pins each stored hash to the hash
// handed back in that seq's 201 response.
func verifyLedger(t *testing.T, path string, want []ledgerRecord, responseHash map[int64]string) []persistedEvent {
	t.Helper()
	events := readFullLedger(t, path)
	if len(events) != len(want) {
		t.Fatalf("persisted %d events, want %d: %+v", len(events), len(want), events)
	}
	prev := strings.Repeat("0", 64)
	for i, expected := range want {
		e := events[i]
		if e.seq != int64(i+1) {
			t.Fatalf("row %d: seq = %d, want %d (chain must stay contiguous)", i, e.seq, i+1)
		}
		if e.account != expected.account || e.operation != expected.operation ||
			e.resource != expected.resource || e.result != expected.result ||
			e.occurredAt != expected.occurredAt {
			t.Fatalf("row %d business fields = %+v, want %+v", i, e, expected)
		}
		if e.prevHash != prev {
			t.Fatalf("row %d prev_hash = %s, want %s", i, e.prevHash, prev)
		}
		recomputed := wantHash(e.seq, expected.account, expected.operation, expected.resource,
			expected.result, expected.occurredAt, e.prevHash)
		if e.hash != recomputed {
			t.Fatalf("row %d hash = %s, want %s per the public rule", i, e.hash, recomputed)
		}
		if e.hash != responseHash[e.seq] {
			t.Fatalf("row %d stored hash = %s, 201 response hash = %s", i, e.hash, responseHash[e.seq])
		}
		prev = e.hash
	}
	return events
}

// assertAccountsAbsent fails if any persisted row belongs to one of the
// named accounts (used to prove losing/failed requests left no record).
func assertAccountsAbsent(t *testing.T, events []persistedEvent, accounts map[string]bool) {
	t.Helper()
	for _, e := range events {
		if accounts[e.account] {
			t.Fatalf("a rejected request (account %q) was persisted at seq %d", e.account, e.seq)
		}
	}
}

func getHealthz(t *testing.T, router http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	return recorder
}

// setInsertFailure installs (or removes) a trigger that fails every INSERT
// on the events table while SELECTs and Ping keep working. It is installed
// from a connection the service does not own, so the product code stays
// unchanged and reads remain available throughout.
func setInsertFailure(t *testing.T, path string, enabled bool) {
	t.Helper()
	statement := "DROP TRIGGER IF EXISTS events_block_insert"
	if enabled {
		statement = `CREATE TRIGGER events_block_insert
			BEFORE INSERT ON events
			BEGIN
				SELECT RAISE(FAIL, 'controlled write failure injected by regression test');
			END;`
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open control connection: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(statement); err != nil {
		t.Fatalf("toggle insert failure (%v): %v", enabled, err)
	}
}

// concurrentFixtures returns distinguishable legal events: mixed offsets
// (including cross-day/month/year conversions), varied fractional-second
// shapes, a double quote, a backslash and non-ASCII text.
func concurrentFixtures() []eventFixture {
	return []eventFixture{
		{eventRequest{Account: `alice "admin"`, Operation: "login", Resource: "console-00", Result: "ok", OccurredAt: "2026-10-05T08:30:00+08:00"}, "2026-10-05T00:30:00Z"},
		{eventRequest{Account: `用户\甲`, Operation: "read", Resource: "console-01", Result: "ok", OccurredAt: "2026-10-04T23:00:00-02:00"}, "2026-10-05T01:00:00Z"},
		{eventRequest{Account: "acct-02", Operation: "write", Resource: "console-02", Result: "denied", OccurredAt: "2026-03-01T00:15:00.25+05:30"}, "2026-02-28T18:45:00.25Z"},
		{eventRequest{Account: "acct-03", Operation: "logout", Resource: "console-03", Result: "ok", OccurredAt: "2026-01-01T00:30:00.5+08:00"}, "2025-12-31T16:30:00.5Z"},
		{eventRequest{Account: "acct-04", Operation: "delete", Resource: "console-04", Result: "error", OccurredAt: "2026-10-05T12:00:00.123456789Z"}, "2026-10-05T12:00:00.123456789Z"},
		{eventRequest{Account: "acct-05", Operation: "grant", Resource: "console-05", Result: "ok", OccurredAt: "2026-09-30T23:59:59-00:30"}, "2026-10-01T00:29:59Z"},
		{eventRequest{Account: "acct-06", Operation: "audit", Resource: "console-06", Result: "ok", OccurredAt: "2026-12-31T23:00:00.000+01:00"}, "2026-12-31T22:00:00.000Z"},
		{eventRequest{Account: "acct-07", Operation: "export", Resource: "console-07", Result: "denied", OccurredAt: "2026-02-28T23:30:00+01:00"}, "2026-02-28T22:30:00Z"},
	}
}

func recordOf(fx eventFixture) ledgerRecord {
	return ledgerRecord{
		account:    fx.req.Account,
		operation:  fx.req.Operation,
		resource:   fx.req.Resource,
		result:     fx.req.Result,
		occurredAt: fx.occurredUTC,
	}
}

func TestPostEventConcurrentAppendsPersistFullChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st := openStoreAt(t, path)
	router := NewRouter(st)
	fixtures := concurrentFixtures()

	// Fire all requests at the same instant on the one service instance.
	// Completion order is deliberately not assumed to be seq order: every
	// assertion below maps responses back through the response's seq.
	start := make(chan struct{})
	recorders := make([]*httptest.ResponseRecorder, len(fixtures))
	var wg sync.WaitGroup
	for i := range fixtures {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			recorders[i] = postStructuredEvent(t, router, fixtures[i].req)
		}(i)
	}
	close(start)
	wg.Wait()

	createdBySeq := map[int64]createdEvent{}
	wantBySeq := map[int64]ledgerRecord{}
	for i, recorder := range recorders {
		if recorder.Code != http.StatusCreated {
			t.Fatalf("request %d: status = %d, want 201 (%s)", i, recorder.Code, recorder.Body.String())
		}
		got := decodeCreated(t, recorder)
		fx := fixtures[i]
		if got.Seq < 1 || got.Seq > int64(len(fixtures)) {
			t.Fatalf("request %d: seq = %d outside 1..%d", i, got.Seq, len(fixtures))
		}
		if _, exists := createdBySeq[got.Seq]; exists {
			t.Fatalf("seq %d handed out more than once", got.Seq)
		}
		// The response carries the submitting request's own fields, stored
		// verbatim, with occurred_at normalized to UTC (fraction kept).
		if got.Account != fx.req.Account || got.Operation != fx.req.Operation ||
			got.Resource != fx.req.Resource || got.Result != fx.req.Result {
			t.Fatalf("request %d: saved business fields %+v, want %+v", i, got, fx.req)
		}
		if got.OccurredAt != fx.occurredUTC {
			t.Fatalf("request %d: occurred_at = %q, want %q", i, got.OccurredAt, fx.occurredUTC)
		}
		// Hash is recomputed independently from the public JSON-array rule.
		if want := wantHash(got.Seq, fx.req.Account, fx.req.Operation, fx.req.Resource,
			fx.req.Result, fx.occurredUTC, got.PrevHash); got.Hash != want {
			t.Fatalf("request %d: hash = %s, want %s", i, got.Hash, want)
		}
		createdBySeq[got.Seq] = got
		wantBySeq[got.Seq] = recordOf(fx)
	}

	// Every request was saved exactly once, with contiguous seqs from 1 and
	// proper prev_hash linkage, regardless of completion order.
	responseHash := map[int64]string{}
	want := make([]ledgerRecord, len(fixtures))
	prev := strings.Repeat("0", 64)
	for seq := int64(1); seq <= int64(len(fixtures)); seq++ {
		got, ok := createdBySeq[seq]
		if !ok {
			t.Fatalf("seq %d missing from responses", seq)
		}
		if got.PrevHash != prev {
			t.Fatalf("seq %d prev_hash = %s, want %s", seq, got.PrevHash, prev)
		}
		if got.Hash != wantHash(seq, wantBySeq[seq].account, wantBySeq[seq].operation,
			wantBySeq[seq].resource, wantBySeq[seq].result, wantBySeq[seq].occurredAt, prev) {
			t.Fatalf("seq %d hash does not verify against the public rule", seq)
		}
		prev = got.Hash
		responseHash[seq] = got.Hash
		want[seq-1] = wantBySeq[seq]
	}
	tail := prev

	// Close, then read the file through an independent connection: the
	// persisted rows match the successful responses one for one.
	closeStore(t, st)
	verifyLedger(t, path, want, responseHash)

	// Reopen: the same chain is visible and the next auto-seq append
	// continues immediately after the old tail.
	st = openStoreAt(t, path)
	recorder := postEventBody(t, NewRouter(st), validEventBody())
	if recorder.Code != http.StatusCreated {
		t.Fatalf("append after reopen: status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	after := decodeCreated(t, recorder)
	if after.Seq != int64(len(fixtures)+1) || after.PrevHash != tail {
		t.Fatalf("after reopen: seq = %d prev_hash = %s, want %d and %s",
			after.Seq, after.PrevHash, len(fixtures)+1, tail)
	}
	want = append(want, ledgerRecord{
		account: "alice", operation: "login", resource: "console", result: "ok",
		occurredAt: "2026-10-05T00:30:00Z",
	})
	responseHash[after.Seq] = after.Hash
	closeStore(t, st)

	// Full records and verification chain are re-checked after close/reopen.
	verifyLedger(t, path, want, responseHash)
}

func TestPostEventExplicitSeqRaceElectsSingleWinner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st := openStoreAt(t, path)
	router := NewRouter(st)

	const contenders = 8
	fixtures := make([]eventFixture, contenders)
	for i := 0; i < contenders; i++ {
		seq := int64(1) // every contender races for the same legal next seq
		marker := string(rune('a' + i))
		fixtures[i] = eventFixture{
			req: eventRequest{
				Account:    "racer-" + marker,
				Operation:  "op-" + marker,
				Resource:   "resource-" + marker,
				Result:     []string{"ok", "denied"}[i%2],
				OccurredAt: "2026-10-05T00:00:0" + string(rune('0'+i)) + "Z",
				Seq:        &seq,
			},
			occurredUTC: "2026-10-05T00:00:0" + string(rune('0'+i)) + "Z",
		}
	}

	start := make(chan struct{})
	recorders := make([]*httptest.ResponseRecorder, contenders)
	var wg sync.WaitGroup
	for i := range fixtures {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			recorders[i] = postStructuredEvent(t, router, fixtures[i].req)
		}(i)
	}
	close(start)
	wg.Wait()

	winners := 0
	var winner createdEvent
	var winnerIndex int
	for i, recorder := range recorders {
		switch recorder.Code {
		case http.StatusCreated:
			winners++
			winner = decodeCreated(t, recorder)
			winnerIndex = i
		case http.StatusConflict:
			assertErrorEnvelope(t, recorder, http.StatusConflict, "audit_seq_conflict")
		default:
			t.Fatalf("contender %d: status = %d, want 201 or 409 (%s)", i, recorder.Code, recorder.Body.String())
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1", winners)
	}
	winnerFX := fixtures[winnerIndex]

	// The unique winner occupies seq 1 with its own content.
	if winner.Seq != 1 || winner.PrevHash != strings.Repeat("0", 64) {
		t.Fatalf("winner = %+v, want seq 1 chained to genesis", winner)
	}
	if winner.Account != winnerFX.req.Account || winner.Operation != winnerFX.req.Operation ||
		winner.Resource != winnerFX.req.Resource || winner.Result != winnerFX.req.Result ||
		winner.OccurredAt != winnerFX.occurredUTC {
		t.Fatalf("winner fields = %+v, want %+v", winner, winnerFX.req)
	}
	if want := wantHash(1, winnerFX.req.Account, winnerFX.req.Operation, winnerFX.req.Resource,
		winnerFX.req.Result, winnerFX.occurredUTC, winner.PrevHash); winner.Hash != want {
		t.Fatalf("winner hash = %s, want %s", winner.Hash, want)
	}

	if recorder := getHealthz(t, router); recorder.Code != http.StatusOK {
		t.Fatalf("healthz after race: status = %d (%s)", recorder.Code, recorder.Body.String())
	}

	loserAccounts := map[string]bool{}
	for i, fx := range fixtures {
		if i != winnerIndex {
			loserAccounts[fx.req.Account] = true
		}
	}

	closeStore(t, st)
	// Only the winner's record exists.
	events := verifyLedger(t, path, []ledgerRecord{recordOf(winnerFX)},
		map[int64]string{1: winner.Hash})
	assertAccountsAbsent(t, events, loserAccounts)

	// After the race the next auto-seq append takes seq 2 and chains to the
	// winner; the losing requests left nothing behind.
	st = openStoreAt(t, path)
	next := eventFixture{
		req: eventRequest{
			Account: "after-race", Operation: "followup", Resource: "console", Result: "ok",
			OccurredAt: "2026-10-05T03:45:00+03:00",
		},
		occurredUTC: "2026-10-05T00:45:00Z",
	}
	recorder := postStructuredEvent(t, NewRouter(st), next.req)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("append after race: status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	followup := decodeCreated(t, recorder)
	if followup.Seq != 2 || followup.PrevHash != winner.Hash {
		t.Fatalf("after race: seq = %d prev_hash = %s, want 2 and %s", followup.Seq, followup.PrevHash, winner.Hash)
	}
	if followup.Account != next.req.Account || followup.OccurredAt != next.occurredUTC {
		t.Fatalf("after race: response = %+v, want %+v", followup, next.req)
	}
	closeStore(t, st)

	events = verifyLedger(t, path,
		[]ledgerRecord{recordOf(winnerFX), recordOf(next)},
		map[int64]string{1: winner.Hash, 2: followup.Hash})
	assertAccountsAbsent(t, events, loserAccounts)
}

func TestPostEventControlledWriteFailureLeavesChainUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st := openStoreAt(t, path)
	router := NewRouter(st)

	// Seed two committed records and snapshot the chain tail.
	seeds := []eventFixture{
		{eventRequest{Account: "alice", Operation: "login", Resource: "console", Result: "ok", OccurredAt: "2026-10-05T08:30:00+08:00"}, "2026-10-05T00:30:00Z"},
		{eventRequest{Account: "bob", Operation: "read", Resource: "report", Result: "denied", OccurredAt: "2026-10-05T01:00:00.250Z"}, "2026-10-05T01:00:00.250Z"},
	}
	want := []ledgerRecord{}
	responseHash := map[int64]string{}
	prev := strings.Repeat("0", 64)
	for i, fx := range seeds {
		recorder := postStructuredEvent(t, router, fx.req)
		if recorder.Code != http.StatusCreated {
			t.Fatalf("seed %d: status = %d (%s)", i, recorder.Code, recorder.Body.String())
		}
		got := decodeCreated(t, recorder)
		if got.Seq != int64(i+1) || got.PrevHash != prev {
			t.Fatalf("seed %d = %+v, want seq %d chained to %s", i, got, i+1, prev)
		}
		if got.OccurredAt != fx.occurredUTC {
			t.Fatalf("seed %d occurred_at = %q", i, got.OccurredAt)
		}
		if h := wantHash(got.Seq, fx.req.Account, fx.req.Operation, fx.req.Resource,
			fx.req.Result, fx.occurredUTC, got.PrevHash); got.Hash != h {
			t.Fatalf("seed %d hash = %s, want %s", i, got.Hash, h)
		}
		prev = got.Hash
		responseHash[got.Seq] = got.Hash
		want = append(want, recordOf(fx))
	}
	tail := prev

	// Make every INSERT fail while the database stays readable. This is
	// deliberately different from the closed-handle scenario covered
	// elsewhere: /healthz must still report ok during the failure.
	setInsertFailure(t, path, true)
	if recorder := getHealthz(t, router); recorder.Code != http.StatusOK {
		t.Fatalf("healthz during write failure: status = %d (%s)", recorder.Code, recorder.Body.String())
	}

	failed := eventRequest{Account: "never-saved", Operation: "write", Resource: "bucket",
		Result: "error", OccurredAt: "2026-10-05T02:00:00Z"}
	recorder := postStructuredEvent(t, router, failed)
	assertErrorEnvelope(t, recorder, http.StatusServiceUnavailable, "storage_unavailable")

	// Existing records, count and chain tail are exactly as before the failure.
	events := verifyLedger(t, path, want, responseHash)
	if events[len(events)-1].hash != tail {
		t.Fatalf("tail hash = %s after failed write, want %s", events[len(events)-1].hash, tail)
	}
	assertAccountsAbsent(t, events, map[string]bool{failed.Account: true})

	// Restore writes: the next append takes the original next seq and chains
	// onto the unchanged tail.
	setInsertFailure(t, path, false)
	if recorder := getHealthz(t, router); recorder.Code != http.StatusOK {
		t.Fatalf("healthz after recovery: status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	recovery := eventFixture{
		req: eventRequest{Account: "carol", Operation: "retry", Resource: "console",
			Result: "ok", OccurredAt: "2026-10-05T03:45:00+03:00"},
		occurredUTC: "2026-10-05T00:45:00Z",
	}
	recorder = postStructuredEvent(t, router, recovery.req)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("append after recovery: status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	got := decodeCreated(t, recorder)
	if got.Seq != int64(len(seeds)+1) || got.PrevHash != tail {
		t.Fatalf("after recovery: seq = %d prev_hash = %s, want %d and %s",
			got.Seq, got.PrevHash, len(seeds)+1, tail)
	}
	wantAfterRecovery := append(append([]ledgerRecord{}, want...), recordOf(recovery))
	responseHash[got.Seq] = got.Hash
	closeStore(t, st)
	events = verifyLedger(t, path, wantAfterRecovery, responseHash)
	assertAccountsAbsent(t, events, map[string]bool{failed.Account: true})

	// After a close/reopen the full chain is intact and still grows, and the
	// failed request remains absent.
	st = openStoreAt(t, path)
	afterReopen := eventFixture{
		req: eventRequest{Account: "dave", Operation: "verify", Resource: "audit-log",
			Result: "ok", OccurredAt: "2026-10-05T00:00:00.5Z"},
		occurredUTC: "2026-10-05T00:00:00.5Z",
	}
	recorder = postStructuredEvent(t, NewRouter(st), afterReopen.req)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("append after reopen: status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	got = decodeCreated(t, recorder)
	if got.Seq != int64(len(seeds)+2) || got.PrevHash != responseHash[int64(len(seeds)+1)] {
		t.Fatalf("after reopen: seq = %d prev_hash = %s", got.Seq, got.PrevHash)
	}
	wantFinal := append(append([]ledgerRecord{}, wantAfterRecovery...), recordOf(afterReopen))
	responseHash[got.Seq] = got.Hash
	closeStore(t, st)

	events = verifyLedger(t, path, wantFinal, responseHash)
	assertAccountsAbsent(t, events, map[string]bool{failed.Account: true})
}

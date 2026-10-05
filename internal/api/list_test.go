package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func getEventsQuery(t *testing.T, router http.Handler, rawQuery string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	target := "/events"
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	request := httptest.NewRequest(http.MethodGet, target, nil)
	router.ServeHTTP(recorder, request)
	return recorder
}

// mustPost appends one event and returns the decoded 201 body.
func mustPost(t *testing.T, router http.Handler, body string) map[string]any {
	t.Helper()
	recorder := postEventBody(t, router, body)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("seed post: status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	return decodeEvent(t, recorder)
}

// eventBody builds a valid append body for the given account and occurred_at.
func eventBody(account, occurredAt string) string {
	return fmt.Sprintf(`{"account":%q,"operation":"op","resource":"res","result":"ok","occurred_at":%q}`, account, occurredAt)
}

// decodePage decodes a 200 GET /events body and checks the top-level shape:
// exactly the events and next_after_seq keys.
func decodePage(t *testing.T, recorder *httptest.ResponseRecorder) (events []map[string]any, next any) {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode page: %v", err)
	}
	if len(decoded) != 2 {
		t.Fatalf("response keys = %v, want only events and next_after_seq", decoded)
	}
	rawEvents, ok := decoded["events"].([]any)
	if !ok {
		t.Fatalf("events = %v, want an array", decoded["events"])
	}
	for _, raw := range rawEvents {
		event, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("event = %v, want an object", raw)
		}
		events = append(events, event)
	}
	return events, decoded["next_after_seq"]
}

// wantSeqs asserts the page carries exactly the given seq values in order.
func wantSeqs(t *testing.T, events []map[string]any, seqs ...float64) {
	t.Helper()
	if len(events) != len(seqs) {
		t.Fatalf("page has %d events, want %d (%v)", len(events), len(seqs), events)
	}
	for i, seq := range seqs {
		if events[i]["seq"] != seq {
			t.Fatalf("event %d seq = %v, want %v", i, events[i]["seq"], seq)
		}
	}
}

// wantInvalidQuery asserts the invalid_audit_input rejection shape for one
// GET /events query string.
func wantInvalidQuery(t *testing.T, router http.Handler, rawQuery string) {
	t.Helper()
	recorder := getEventsQuery(t, router, rawQuery)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("query %q: status = %d, want 400 (%s)", rawQuery, recorder.Code, recorder.Body.String())
	}
	var decoded struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("query %q: decode error body: %v", rawQuery, err)
	}
	if decoded.Error.Code != "invalid_audit_input" {
		t.Fatalf("query %q: code = %q", rawQuery, decoded.Error.Code)
	}
	if decoded.Error.Message == "" {
		t.Fatalf("query %q: message must be non-empty", rawQuery)
	}
}

func TestGetEventsEmptyLedger(t *testing.T) {
	router := NewRouter(openTestStore(t))

	recorder := getEventsQuery(t, router, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"events":[]`) {
		t.Fatalf("body = %s, want an empty events array", recorder.Body.String())
	}
	events, next := decodePage(t, recorder)
	if len(events) != 0 || next != nil {
		t.Fatalf("events = %v next = %v, want empty page and null cursor", events, next)
	}
}

func TestGetEventsReturnsStoredRecordsInSeqOrder(t *testing.T) {
	router := NewRouter(openTestStore(t))

	first := mustPost(t, router, eventBody("  alice  ", "2026-10-05T08:30:00+08:00"))
	second := mustPost(t, router, eventBody("bob", "2026-10-05T01:00:00.2500Z"))

	recorder := getEventsQuery(t, router, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	events, next := decodePage(t, recorder)
	wantSeqs(t, events, 1, 2)
	if next != nil {
		t.Fatalf("next_after_seq = %v, want null", next)
	}
	// Each item reuses the append response shape and keeps the stored text.
	for i, posted := range []map[string]any{first, second} {
		for _, key := range []string{"account", "operation", "resource", "result", "occurred_at", "seq", "prev_hash", "hash"} {
			if events[i][key] != posted[key] {
				t.Fatalf("event %d %s = %v, want %v", i, key, events[i][key], posted[key])
			}
		}
		if len(events[i]) != 8 {
			t.Fatalf("event %d keys = %v, want the 8 append response fields", i, events[i])
		}
	}
	if events[0]["account"] != "  alice  " || events[1]["occurred_at"] != "2026-10-05T01:00:00.2500Z" {
		t.Fatalf("stored originals not preserved: %v", events)
	}
}

func TestGetEventsFiltersByAccountExactly(t *testing.T) {
	router := NewRouter(openTestStore(t))

	mustPost(t, router, eventBody("alice", "2026-10-05T00:00:00Z"))
	mustPost(t, router, eventBody("Alice", "2026-10-05T00:01:00Z"))
	mustPost(t, router, eventBody(" alice ", "2026-10-05T00:02:00Z"))
	mustPost(t, router, eventBody("bob", "2026-10-05T00:03:00Z"))

	// Exact, case-sensitive match on the decoded text.
	events, _ := decodePage(t, getEventsQuery(t, router, "account=alice"))
	wantSeqs(t, events, 1)
	events, _ = decodePage(t, getEventsQuery(t, router, "account=Alice"))
	wantSeqs(t, events, 2)
	// Surrounding whitespace is part of the value.
	events, _ = decodePage(t, getEventsQuery(t, router, "account=%20alice%20"))
	wantSeqs(t, events, 3)
	// No match yields an empty page and a null cursor.
	events, next := decodePage(t, getEventsQuery(t, router, "account=ALICE"))
	if len(events) != 0 || next != nil {
		t.Fatalf("events = %v next = %v, want empty page and null cursor", events, next)
	}
}

func TestGetEventsRejectsBlankAccount(t *testing.T) {
	router := NewRouter(openTestStore(t))
	for _, query := range []string{"account=", "account", "account=%20", "account=%09%20"} {
		wantInvalidQuery(t, router, query)
	}
}

func TestGetEventsTimeWindow(t *testing.T) {
	router := NewRouter(openTestStore(t))

	mustPost(t, router, eventBody("a", "2026-10-05T00:00:00Z"))
	mustPost(t, router, eventBody("a", "2026-10-05T01:00:00.250Z"))
	mustPost(t, router, eventBody("a", "2026-10-05T02:00:00Z"))

	// from is inclusive, to is exclusive; ".25" names the same instant as
	// the stored ".250".
	events, _ := decodePage(t, getEventsQuery(t, router,
		"from=2026-10-05T01:00:00.25Z&to=2026-10-05T02:00:00Z"))
	wantSeqs(t, events, 2)

	// A bound given with an offset names the same instant as its UTC form.
	events, _ = decodePage(t, getEventsQuery(t, router,
		"from=2026-10-05T09:00:00.250%2B08:00&to=2026-10-05T10:00:00%2B08:00"))
	wantSeqs(t, events, 2)

	// Only a lower bound, and only an upper bound.
	events, _ = decodePage(t, getEventsQuery(t, router, "from=2026-10-05T00:30:00Z"))
	wantSeqs(t, events, 2, 3)
	events, _ = decodePage(t, getEventsQuery(t, router, "to=2026-10-05T01:00:00.250Z"))
	wantSeqs(t, events, 1)

	// A window that matches nothing.
	events, next := decodePage(t, getEventsQuery(t, router,
		"from=2026-10-06T00:00:00Z&to=2026-10-07T00:00:00Z"))
	if len(events) != 0 || next != nil {
		t.Fatalf("events = %v next = %v, want empty page and null cursor", events, next)
	}
}

func TestGetEventsTimeWindowFractionPrecision(t *testing.T) {
	router := NewRouter(openTestStore(t))

	// A fraction longer than nanosecond precision is stored verbatim.
	mustPost(t, router, eventBody("a", "2026-10-05T00:00:00.000000000000001Z"))
	mustPost(t, router, eventBody("a", "2026-10-05T00:00:00.5Z"))

	// The record sorts after a bound one tick below it and before one above.
	events, _ := decodePage(t, getEventsQuery(t, router, "from=2026-10-05T00:00:00.000000000000002Z"))
	wantSeqs(t, events, 2)
	events, _ = decodePage(t, getEventsQuery(t, router, "to=2026-10-05T00:00:00.000000000000001Z"))
	if len(events) != 0 {
		t.Fatalf("events = %v, want none before the first record's instant", events)
	}
	// ".5" includes a record at exactly ".5" as the lower bound…
	events, _ = decodePage(t, getEventsQuery(t, router, "from=2026-10-05T00:00:00.4999Z&to=2026-10-05T00:00:00.5001Z"))
	wantSeqs(t, events, 2)
	// …but ".50" names the same instant and excludes it as the upper bound.
	events, _ = decodePage(t, getEventsQuery(t, router, "from=2026-10-05T00:00:00.4999Z&to=2026-10-05T00:00:00.50Z"))
	if len(events) != 0 {
		t.Fatalf("events = %v, want the record at .5 excluded by the equal upper bound", events)
	}
}

func TestGetEventsRejectsInvalidTimeBounds(t *testing.T) {
	router := NewRouter(openTestStore(t))
	cases := []string{
		"from=notatime",
		"from=2026-10-05T00:00:00",                                 // no zone
		"from=2026-13-01T00:00:00Z",                                // bad month
		"from=2016-12-31T23:59:60Z",                                // leap second
		"from=0000-01-01T00:00:00%2B00:01",                         // UTC year below 0000
		"to=9999-12-31T23:59:59.5-00:01",                           // UTC year above 9999
		"from=",                                                    // explicit empty
		"to=2026-10-05T00:00:00Z&from=2026-10-05T00:00:01Z",        // from after to
		"from=2026-10-05T00:00:00Z&to=2026-10-05T00:00:00Z",        // from equal to to
		"from=2026-10-05T08:00:00%2B08:00&to=2026-10-05T00:00:00Z", // same instant, different spelling
		"from=2026-10-05T00:00:00.5Z&to=2026-10-05T00:00:00.50Z",   // same instant, different fraction
	}
	for _, query := range cases {
		wantInvalidQuery(t, router, query)
	}
}

func TestGetEventsPagination(t *testing.T) {
	router := NewRouter(openTestStore(t))
	for i := 0; i < 5; i++ {
		mustPost(t, router, eventBody("a", fmt.Sprintf("2026-10-05T00:0%d:00Z", i)))
	}

	// First page: limit records and a cursor pointing at the last one.
	events, next := decodePage(t, getEventsQuery(t, router, "limit=2"))
	wantSeqs(t, events, 1, 2)
	if next != float64(2) {
		t.Fatalf("next_after_seq = %v, want 2", next)
	}

	// Second page continues strictly after the cursor.
	events, next = decodePage(t, getEventsQuery(t, router, "limit=2&after_seq=2"))
	wantSeqs(t, events, 3, 4)
	if next != float64(4) {
		t.Fatalf("next_after_seq = %v, want 4", next)
	}

	// Last page is short and closes the cursor.
	events, next = decodePage(t, getEventsQuery(t, router, "limit=2&after_seq=4"))
	wantSeqs(t, events, 5)
	if next != nil {
		t.Fatalf("next_after_seq = %v, want null", next)
	}

	// A cursor past the end yields an empty page and a null cursor.
	events, next = decodePage(t, getEventsQuery(t, router, "after_seq=99"))
	if len(events) != 0 || next != nil {
		t.Fatalf("events = %v next = %v, want empty page and null cursor", events, next)
	}

	// Leading zeros are accepted; the maximum int64 cursor is valid.
	events, _ = decodePage(t, getEventsQuery(t, router, "after_seq=003&limit=007"))
	wantSeqs(t, events, 4, 5)
	events, next = decodePage(t, getEventsQuery(t, router, "after_seq=9223372036854775807"))
	if len(events) != 0 || next != nil {
		t.Fatalf("events = %v next = %v, want empty page and null cursor", events, next)
	}

	// A page that ends exactly on the last record closes the cursor.
	events, next = decodePage(t, getEventsQuery(t, router, "limit=5"))
	wantSeqs(t, events, 1, 2, 3, 4, 5)
	if next != nil {
		t.Fatalf("next_after_seq = %v, want null", next)
	}
}

func TestGetEventsDefaultLimitIsFifty(t *testing.T) {
	router := NewRouter(openTestStore(t))
	for i := 0; i < 55; i++ {
		mustPost(t, router, eventBody("a", fmt.Sprintf("2026-10-05T00:%02d:00Z", i)))
	}

	events, next := decodePage(t, getEventsQuery(t, router, ""))
	if len(events) != 50 {
		t.Fatalf("page has %d events, want the default limit of 50", len(events))
	}
	if next != float64(50) {
		t.Fatalf("next_after_seq = %v, want 50", next)
	}
	events, next = decodePage(t, getEventsQuery(t, router, "after_seq=50"))
	if len(events) != 5 || next != nil {
		t.Fatalf("second page = %d events, next = %v; want 5 events and null", len(events), next)
	}
}

func TestGetEventsRejectsInvalidPagination(t *testing.T) {
	router := NewRouter(openTestStore(t))
	cases := []string{
		"limit=", "limit=0", "limit=101", "limit=-1", "limit=1.5", "limit=1e2",
		"limit=%2B5", "limit=5%20", "limit=99999999999999999999",
		"after_seq=", "after_seq=-1", "after_seq=1.5", "after_seq=abc",
		"after_seq=9223372036854775808",
	}
	for _, query := range cases {
		wantInvalidQuery(t, router, query)
	}
}

func TestGetEventsRejectsMalformedQueryStrings(t *testing.T) {
	router := NewRouter(openTestStore(t))
	cases := []string{
		"limit=1&limit=2",     // repeated parameter
		"account=a&account=a", // repeated, same value
		"from=2026-10-05T00:00:00Z&from=2026-10-05T00:00:00Z",
		"foo=1",         // unknown parameter
		"limits=5",      // unknown parameter
		"limit=%zz",     // bad percent escape
		"account=%ff",   // decodes to invalid UTF-8
		"account=a;b=1", // stray semicolon
	}
	for _, query := range cases {
		wantInvalidQuery(t, router, query)
	}
}

func TestGetEventsParamErrorBeatsStorageError(t *testing.T) {
	st := openTestStore(t)
	router := NewRouter(st)
	st.Close()

	// Even with storage down, an invalid query is a 400.
	wantInvalidQuery(t, router, "limit=abc")
	wantInvalidQuery(t, router, "from=2026-10-05T00:00:00Z&to=2026-10-05T00:00:00Z")

	// A valid query against unavailable storage is a 503.
	recorder := getEventsQuery(t, router, "account=alice")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (%s)", recorder.Code, recorder.Body.String())
	}
	if event := decodeEvent(t, recorder); event["error"].(map[string]any)["code"] != "storage_unavailable" {
		t.Fatalf("body = %v", event)
	}
}

func TestGetEventsCombinesFiltersAndCursor(t *testing.T) {
	router := NewRouter(openTestStore(t))

	mustPost(t, router, eventBody("alice", "2026-10-05T00:00:00Z"))
	mustPost(t, router, eventBody("bob", "2026-10-05T01:00:00Z"))
	mustPost(t, router, eventBody("alice", "2026-10-05T02:00:00Z"))
	mustPost(t, router, eventBody("alice", "2026-10-05T03:00:00Z"))

	// The account filter, the window and the cursor all apply together.
	events, next := decodePage(t, getEventsQuery(t, router,
		"account=alice&from=2026-10-05T00:00:00Z&to=2026-10-05T03:00:00Z&limit=1"))
	wantSeqs(t, events, 1)
	if next != float64(1) {
		t.Fatalf("next_after_seq = %v, want 1", next)
	}
	events, next = decodePage(t, getEventsQuery(t, router,
		"account=alice&from=2026-10-05T00:00:00Z&to=2026-10-05T03:00:00Z&limit=1&after_seq=1"))
	wantSeqs(t, events, 3)
	if next != nil {
		t.Fatalf("next_after_seq = %v, want null", next)
	}
}

func TestGetEventsLaterPagesSeeLaterAppends(t *testing.T) {
	router := NewRouter(openTestStore(t))
	mustPost(t, router, eventBody("a", "2026-10-05T00:00:00Z"))
	mustPost(t, router, eventBody("a", "2026-10-05T00:01:00Z"))

	events, next := decodePage(t, getEventsQuery(t, router, "limit=2"))
	wantSeqs(t, events, 1, 2)
	if next != nil {
		t.Fatalf("next_after_seq = %v, want null", next)
	}

	// A record appended after the first page is visible to a later page.
	mustPost(t, router, eventBody("a", "2026-10-05T00:02:00Z"))
	events, next = decodePage(t, getEventsQuery(t, router, "limit=2&after_seq=2"))
	wantSeqs(t, events, 3)
	if next != nil {
		t.Fatalf("next_after_seq = %v, want null", next)
	}
}

func TestGetEventsDoesNotTouchTheLedger(t *testing.T) {
	router := NewRouter(openTestStore(t))
	mustPost(t, router, eventBody("alice", "2026-10-05T00:00:00Z"))
	mustPost(t, router, eventBody("bob", "2026-10-05T01:00:00Z"))

	for _, query := range []string{"", "account=alice", "from=2026-10-05T00:00:00Z", "limit=1"} {
		if recorder := getEventsQuery(t, router, query); recorder.Code != http.StatusOK {
			t.Fatalf("query %q: status = %d", query, recorder.Code)
		}
	}

	// The chain still verifies and no record was consumed or rewritten.
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/ledger/verify", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("verify: status = %d", recorder.Code)
	}
	result := decodeEvent(t, recorder)
	if result["valid"] != true || result["checked"] != float64(2) {
		t.Fatalf("verify = %v, want a valid chain of 2 records", result)
	}
}

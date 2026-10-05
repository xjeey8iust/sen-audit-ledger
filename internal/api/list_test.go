package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// getEventsRaw issues GET /events with the query string placed verbatim into
// RawQuery, so tests can exercise undecodable escapes and other raw forms a
// client-side URL parser would reject before the request reached the router.
func getEventsRaw(t *testing.T, router http.Handler, rawQuery string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/events", nil)
	request.URL.RawQuery = rawQuery
	router.ServeHTTP(recorder, request)
	return recorder
}

// listBody is the decoded GET /events success body.
type listBody struct {
	Events       []map[string]any
	NextAfterSeq *int64
}

// decodeList decodes a 200 list body and checks the top-level shape: exactly
// the events and next_after_seq keys, with events an array.
func decodeList(t *testing.T, recorder *httptest.ResponseRecorder) listBody {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
	decoded := decodeEvent(t, recorder)
	if len(decoded) != 2 {
		t.Fatalf("response keys = %v, want only events and next_after_seq", decoded)
	}
	var page listBody
	rawEvents, ok := decoded["events"].([]any)
	if !ok {
		t.Fatalf("events = %v, want an array", decoded["events"])
	}
	for _, item := range rawEvents {
		event, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("event item = %v, want an object", item)
		}
		page.Events = append(page.Events, event)
	}
	if decoded["next_after_seq"] != nil {
		cursor, ok := decoded["next_after_seq"].(float64)
		if !ok {
			t.Fatalf("next_after_seq = %v, want a number or null", decoded["next_after_seq"])
		}
		seq := int64(cursor)
		page.NextAfterSeq = &seq
	}
	return page
}

// listSeqs extracts the seq values of one decoded page, in order.
func listSeqs(page listBody) []int64 {
	seqs := make([]int64, 0, len(page.Events))
	for _, event := range page.Events {
		seqs = append(seqs, int64(event["seq"].(float64)))
	}
	return seqs
}

// wantSeqs fails unless got is exactly want, in order.
func wantSeqs(t *testing.T, got []int64, want ...int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("seqs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("seqs = %v, want %v", got, want)
		}
	}
}

// seedListEvents appends one record per (account, occurredAt) pair.
func seedListEvents(t *testing.T, router http.Handler, occurredAts ...string) []map[string]any {
	t.Helper()
	events := make([]map[string]any, 0, len(occurredAts))
	for _, occurredAt := range occurredAts {
		body := fmt.Sprintf(`{"account":"a","operation":"op","resource":"res","result":"ok","occurred_at":%q}`, occurredAt)
		recorder := postEventBody(t, router, body)
		if recorder.Code != http.StatusCreated {
			t.Fatalf("seed %s: status = %d (%s)", occurredAt, recorder.Code, recorder.Body.String())
		}
		events = append(events, decodeEvent(t, recorder))
	}
	return events
}

// wantInvalidQuery requests one page and asserts the invalid_audit_input
// rejection shape: 400, only a top-level error object, and no internals.
func wantInvalidQuery(t *testing.T, router http.Handler, rawQuery string) {
	t.Helper()
	recorder := getEventsRaw(t, router, rawQuery)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("query %q: status = %d, want %d (%s)", rawQuery, recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
	decoded := decodeEvent(t, recorder)
	if len(decoded) != 1 {
		t.Fatalf("query %q: response keys = %v, want only the error object", rawQuery, decoded)
	}
	errObj, ok := decoded["error"].(map[string]any)
	if !ok {
		t.Fatalf("query %q: error = %v, want an object", rawQuery, decoded["error"])
	}
	if errObj["code"] != "invalid_audit_input" {
		t.Fatalf("query %q: code = %v, want invalid_audit_input", rawQuery, errObj["code"])
	}
	if message, ok := errObj["message"].(string); !ok || message == "" {
		t.Fatalf("query %q: message = %v, want a non-empty string", rawQuery, errObj["message"])
	}
	leaks := []string{"sql", "SELECT", "goroutine", ".go", "/", "\\"}
	for _, leak := range leaks {
		if strings.Contains(recorder.Body.String(), leak) {
			t.Fatalf("query %q: response leaks internals (%q): %s", rawQuery, leak, recorder.Body.String())
		}
	}
}

func TestGetEventsEmptyLedger(t *testing.T) {
	router := NewRouter(openTestStore(t))

	page := decodeList(t, getEventsRaw(t, router, ""))
	if len(page.Events) != 0 {
		t.Fatalf("events = %v, want empty", page.Events)
	}
	if page.NextAfterSeq != nil {
		t.Fatalf("next_after_seq = %v, want null", *page.NextAfterSeq)
	}
	// The empty page is a JSON array, not null.
	if !strings.Contains(recorderBody(t, router, ""), `"events":[]`) {
		t.Fatal("empty page must render events as []")
	}
}

func recorderBody(t *testing.T, router http.Handler, rawQuery string) string {
	t.Helper()
	return getEventsRaw(t, router, rawQuery).Body.String()
}

func TestGetEventsPagination(t *testing.T) {
	router := NewRouter(openTestStore(t))
	seedListEvents(t, router,
		"2026-10-01T00:00:00Z", "2026-10-02T00:00:00Z", "2026-10-03T00:00:00Z",
		"2026-10-04T00:00:00Z", "2026-10-05T00:00:00Z")

	first := decodeList(t, getEventsRaw(t, router, "limit=2"))
	wantSeqs(t, listSeqs(first), 1, 2)
	if first.NextAfterSeq == nil || *first.NextAfterSeq != 2 {
		t.Fatalf("first page next_after_seq = %v, want 2", first.NextAfterSeq)
	}

	second := decodeList(t, getEventsRaw(t, router, "limit=2&after_seq=2"))
	wantSeqs(t, listSeqs(second), 3, 4)
	if second.NextAfterSeq == nil || *second.NextAfterSeq != 4 {
		t.Fatalf("second page next_after_seq = %v, want 4", second.NextAfterSeq)
	}

	third := decodeList(t, getEventsRaw(t, router, "limit=2&after_seq=4"))
	wantSeqs(t, listSeqs(third), 5)
	if third.NextAfterSeq != nil {
		t.Fatalf("last page next_after_seq = %v, want null", *third.NextAfterSeq)
	}

	// A cursor at or past the last record yields an empty page.
	past := decodeList(t, getEventsRaw(t, router, "after_seq=5"))
	wantSeqs(t, listSeqs(past))
	if past.NextAfterSeq != nil {
		t.Fatalf("past-the-end next_after_seq = %v, want null", *past.NextAfterSeq)
	}

	// Leading zeros are accepted on both numeric parameters.
	zeros := decodeList(t, getEventsRaw(t, router, "limit=002&after_seq=004"))
	wantSeqs(t, listSeqs(zeros), 5)
}

func TestGetEventsDefaultLimit(t *testing.T) {
	router := NewRouter(openTestStore(t))
	times := make([]string, 0, 55)
	for i := 0; i < 55; i++ {
		times = append(times, fmt.Sprintf("2026-10-05T00:%02d:00Z", i))
	}
	seedListEvents(t, router, times...)

	page := decodeList(t, getEventsRaw(t, router, ""))
	if len(page.Events) != 50 {
		t.Fatalf("default page size = %d, want 50", len(page.Events))
	}
	if page.NextAfterSeq == nil || *page.NextAfterSeq != 50 {
		t.Fatalf("next_after_seq = %v, want 50", page.NextAfterSeq)
	}

	rest := decodeList(t, getEventsRaw(t, router, "after_seq=50"))
	if len(rest.Events) != 5 || rest.NextAfterSeq != nil {
		t.Fatalf("second page = %d events, cursor %v; want 5 events and null", len(rest.Events), rest.NextAfterSeq)
	}
}

func TestGetEventsAccountFilter(t *testing.T) {
	router := NewRouter(openTestStore(t))
	accounts := []string{"alice", "Alice", " alice ", "账号"}
	for _, account := range accounts {
		body := fmt.Sprintf(`{"account":%s,"operation":"op","resource":"res","result":"ok","occurred_at":"2026-10-05T00:00:00Z"}`,
			strconv.Quote(account))
		if recorder := postEventBody(t, router, body); recorder.Code != http.StatusCreated {
			t.Fatalf("seed %q: status = %d (%s)", account, recorder.Code, recorder.Body.String())
		}
	}

	// The decoded text matches byte for byte: case and whitespace matter.
	cases := []struct {
		query string
		want  []int64
	}{
		{"account=alice", []int64{1}},
		{"account=Alice", []int64{2}},
		{"account=%20alice%20", []int64{3}},
		{"account=ALICE", nil},
		{"account=bob", nil},
		{"account=" + "%E8%B4%A6%E5%8F%B7", []int64{4}}, // 账号
	}
	for _, tc := range cases {
		page := decodeList(t, getEventsRaw(t, router, tc.query))
		wantSeqs(t, listSeqs(page), tc.want...)
		if page.NextAfterSeq != nil {
			t.Fatalf("%s: next_after_seq = %v, want null", tc.query, *page.NextAfterSeq)
		}
	}
}

func TestGetEventsTimeWindow(t *testing.T) {
	router := NewRouter(openTestStore(t))
	seedListEvents(t, router,
		"2026-10-05T00:00:00Z",            // seq 1
		"2026-10-05T00:00:00.0000000001Z", // seq 2
		"2026-10-05T00:00:00.5Z",          // seq 3
		"2026-10-05T01:00:00Z",            // seq 4
	)

	cases := []struct {
		name  string
		query string
		want  []int64
	}{
		// from is inclusive and fractions of any length participate.
		{"from inclusive long fraction", "from=2026-10-05T00:00:00.0000000001Z", []int64{2, 3, 4}},
		{"from below long fraction", "from=2026-10-05T00:00:00.00000000001Z", []int64{2, 3, 4}},
		{"from above long fraction", "from=2026-10-05T00:00:00.0000000002Z", []int64{3, 4}},
		// Equal instants written differently compare equal.
		{"from equal other writing", "from=2026-10-05T00:00:00.50Z", []int64{3, 4}},
		{"from just above other writing", "from=2026-10-05T00:00:00.500000000001Z", []int64{4}},
		// to is exclusive.
		{"to exclusive", "to=2026-10-05T00:00:00.5Z", []int64{1, 2}},
		{"to exclusive other writing", "to=2026-10-05T00:00:00.500Z", []int64{1, 2}},
		// Offset forms normalize to the same instant before comparison.
		{"from with offset", "from=2026-10-05T08:00:00%2B08:00", []int64{1, 2, 3, 4}},
		{"to with offset", "to=2026-10-05T08:00:00.5%2B08:00", []int64{1, 2}},
		// A bounded window.
		{"window", "from=2026-10-05T00:00:00.0000000001Z&to=2026-10-05T00:00:00.5Z", []int64{2}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page := decodeList(t, getEventsRaw(t, router, tc.query))
			wantSeqs(t, listSeqs(page), tc.want...)
		})
	}
}

func TestGetEventsRejectsInvalidParams(t *testing.T) {
	cases := map[string]string{
		"duplicate limit":              "limit=1&limit=2",
		"duplicate account":            "account=a&account=b",
		"unknown parameter":            "page=1",
		"empty limit":                  "limit=",
		"bare account":                 "account",
		"empty from":                   "from=",
		"blank account":                "account=%20%20",
		"tab-only account":             "account=%09",
		"undecodable escape":           "account=%zz",
		"semicolon separator":          "account=a;limit=1",
		"limit zero":                   "limit=0",
		"limit above max":              "limit=101",
		"limit negative":               "limit=-1",
		"limit fraction":               "limit=1.5",
		"limit exponent":               "limit=1e2",
		"limit text":                   "limit=abc",
		"limit explicit plus":          "limit=%2B5",
		"limit overflow":               "limit=99999999999999999999999999",
		"after_seq negative":           "after_seq=-1",
		"after_seq text":               "after_seq=abc",
		"after_seq fraction":           "after_seq=1.0",
		"after_seq overflow":           "after_seq=9223372036854775808",
		"from missing zone":            "from=2026-10-05T00:00:00",
		"from leap second":             "from=2016-12-31T23:59:60Z",
		"from bad date":                "from=2026-02-30T00:00:00Z",
		"from empty fraction":          "from=2026-10-05T00:00:00.Z",
		"to below utc range":           "to=0000-01-01T00:00:00%2B00:01",
		"to above utc range":           "to=9999-12-31T23:59:59-00:01",
		"from after to":                "from=2026-10-06T00:00:00Z&to=2026-10-05T00:00:00Z",
		"from equals to":               "from=2026-10-05T00:00:00Z&to=2026-10-05T00:00:00Z",
		"from equals to other writing": "from=2026-10-05T00:00:00.5Z&to=2026-10-05T08:00:00.50%2B08:00",
	}
	for name, rawQuery := range cases {
		t.Run(name, func(t *testing.T) {
			router := NewRouter(openTestStore(t))
			wantInvalidQuery(t, router, rawQuery)
		})
	}
}

func TestGetEventsResponseShape(t *testing.T) {
	router := NewRouter(openTestStore(t))
	body := `{"account":"  alice  ","operation":"login","resource":"console","result":"ok","occurred_at":"2026-10-05T08:30:00.250+08:00"}`
	appended := postEventBody(t, router, body)
	if appended.Code != http.StatusCreated {
		t.Fatalf("seed: status = %d (%s)", appended.Code, appended.Body.String())
	}
	want := decodeEvent(t, appended)

	page := decodeList(t, getEventsRaw(t, router, ""))
	if len(page.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(page.Events))
	}
	event := page.Events[0]
	// The item reuses the append response structure: exactly these fields.
	wantKeys := []string{"account", "operation", "resource", "result", "occurred_at", "seq", "prev_hash", "hash"}
	if len(event) != len(wantKeys) {
		t.Fatalf("event keys = %v, want %v", event, wantKeys)
	}
	for _, key := range wantKeys {
		if event[key] != want[key] {
			t.Fatalf("event[%q] = %v, want the appended value %v", key, event[key], want[key])
		}
	}
	// Stored text and the normalized timestamp survive the round trip.
	if event["account"] != "  alice  " || event["occurred_at"] != "2026-10-05T00:30:00.250Z" {
		t.Fatalf("event = %v, want original account text and normalized time", event)
	}
}

func TestGetEventsSeesLaterAppends(t *testing.T) {
	router := NewRouter(openTestStore(t))
	seedListEvents(t, router, "2026-10-01T00:00:00Z", "2026-10-02T00:00:00Z", "2026-10-03T00:00:00Z")

	first := decodeList(t, getEventsRaw(t, router, "limit=2"))
	if first.NextAfterSeq == nil || *first.NextAfterSeq != 2 {
		t.Fatalf("next_after_seq = %v, want 2", first.NextAfterSeq)
	}

	// Records appended after a page was served show up in later pages.
	seedListEvents(t, router, "2026-10-04T00:00:00Z")
	second := decodeList(t, getEventsRaw(t, router, "limit=2&after_seq=2"))
	wantSeqs(t, listSeqs(second), 3, 4)
	if second.NextAfterSeq != nil {
		t.Fatalf("next_after_seq = %v, want null", *second.NextAfterSeq)
	}
}

func TestGetEventsDoesNotModifyLedger(t *testing.T) {
	router := NewRouter(openTestStore(t))
	appended := seedListEvents(t, router, "2026-10-01T00:00:00Z", "2026-10-02T00:00:00Z")

	page := decodeList(t, getEventsRaw(t, router, "account=a"))
	if len(page.Events) != 2 {
		t.Fatalf("events = %d, want 2", len(page.Events))
	}
	// The listed records carry the hashes assigned at append time.
	for i, event := range page.Events {
		if event["hash"] != appended[i]["hash"] || event["prev_hash"] != appended[i]["prev_hash"] {
			t.Fatalf("event %d hash pair changed between append and list", i+1)
		}
	}

	// The chain still verifies after the read.
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/ledger/verify", nil)
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("verify: status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	result := decodeEvent(t, recorder)
	if result["valid"] != true || result["checked"] != float64(2) {
		t.Fatalf("verify after list = %v, want valid with 2 checked", result)
	}
}

func TestGetEventsStorageFailure(t *testing.T) {
	st := openTestStore(t)
	router := NewRouter(st)
	st.Close()

	recorder := getEventsRaw(t, router, "")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (%s)", recorder.Code, recorder.Body.String())
	}
	decoded := decodeEvent(t, recorder)
	if decoded["error"].(map[string]any)["code"] != "storage_unavailable" {
		t.Fatalf("body = %v", decoded)
	}
	// No partial page leaks alongside the error.
	if _, present := decoded["events"]; present {
		t.Fatalf("error body must not carry events: %v", decoded)
	}
}

func TestGetEventsParamErrorBeatsStorageError(t *testing.T) {
	st := openTestStore(t)
	router := NewRouter(st)
	st.Close()

	// Storage is down, but the invalid parameter is reported first.
	wantInvalidQuery(t, router, "limit=0")
	wantInvalidQuery(t, router, "from=not-a-time")
	wantInvalidQuery(t, router, "unknown=1")
}

func TestGetEventsErrorMessageShape(t *testing.T) {
	st := openTestStore(t)
	router := NewRouter(st)
	st.Close()

	recorder := getEventsRaw(t, router, "")
	body := recorder.Body.String()
	leaks := []string{"sql", "SELECT", "goroutine", ".go", "/", "\\"}
	for _, leak := range leaks {
		if strings.Contains(body, leak) {
			t.Fatalf("503 body leaks internals (%q): %s", leak, body)
		}
	}
	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if message := decoded["error"].(map[string]any)["message"]; message == "" {
		t.Fatal("503 message must be non-empty")
	}
}

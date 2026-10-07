package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/xjeey8iust/sen-audit-ledger/internal/store"
)

// The combined pagination regression works against one fixed input table:
// 104 records match account "carol" inside the main window
// [combinedFrom, combinedTo), interleaved with 19 records that each break
// exactly one condition (account case, surrounding whitespace, or the time
// window). Expected pages are derived from this table and the published
// semantics — exact account text, from inclusive, to exclusive, ascending
// seq — never from a query response.
const (
	combinedAccount = "carol"
	combinedFrom    = "2026-10-05T00:00:00Z"
	combinedTo      = "2026-10-06T00:00:00Z"
)

// combinedSeed is one predefined append input; match records whether the
// seed satisfies the main query by the public semantics.
type combinedSeed struct {
	account    string
	occurredAt string
	match      bool
}

// combinedSeeds returns the fixed input table. Matching records carry
// distinct in-window instants permuted so that time order differs from
// append order; non-matching records are interleaved in clusters, closing
// with a non-matching tail run after the last match.
func combinedSeeds() []combinedSeed {
	matches := make([]combinedSeed, 0, 104)
	// 101 in-window instants on distinct minutes: i*17 is coprime with the
	// 1439 minutes of the day, so the minutes are unique and shuffled.
	for i := 0; i < 101; i++ {
		minute := (i * 17) % 1439
		matches = append(matches, combinedSeed{
			account:    combinedAccount,
			occurredAt: "2026-10-05T" + twoDigits(minute/60) + ":" + twoDigits(minute%60) + ":00Z",
			match:      true,
		})
	}
	// Special matching records: a fraction longer than nanoseconds, a
	// trailing-zero fraction, and an offset spelling of an in-window instant.
	matches = append(matches,
		combinedSeed{combinedAccount, "2026-10-05T12:00:00.000000000000001Z", true},
		combinedSeed{combinedAccount, "2026-10-05T06:00:00.2500Z", true},
		combinedSeed{combinedAccount, "2026-10-05T09:30:00+08:00", true},
	)
	nonMatches := []combinedSeed{
		{"Carol", "2026-10-05T10:00:00Z", false},
		{"CAROL", "2026-10-05T11:00:00Z", false},
		{" carol", "2026-10-05T12:30:00Z", false},
		{"carol ", "2026-10-05T13:30:00Z", false},
		{"  carol  ", "2026-10-05T14:30:00Z", false},
		{combinedAccount, "2026-10-06T00:00:00Z", false},                 // exactly at to: excluded
		{combinedAccount, "2026-10-04T23:59:59.999999999999999Z", false}, // just before from
		{combinedAccount, "2026-10-06T00:00:00.000000000000001Z", false}, // just after to
		{combinedAccount, "2026-10-04T12:00:00Z", false},
		{combinedAccount, "2026-10-06T12:00:00Z", false},
		{"dave", "2026-10-05T08:00:00Z", false},
		{"dave", "2026-10-05T09:00:00Z", false},
		{"dave", "2026-10-05T15:00:00Z", false},
		{"dave", "2026-10-05T16:00:00Z", false},
		{"eve", "2026-10-05T17:00:00Z", false},
		{"CAROL ", "2026-10-05T18:00:00Z", false},
		{"dave", "2026-10-04T10:00:00Z", false},
		{"eve", "2026-10-06T10:00:00Z", false},
		{"Carol", "2026-10-05T19:00:00Z", false},
	}
	// Interleave matching chunks with non-matching clusters (one cluster of
	// six in the middle) and end with a non-matching tail run of four.
	pattern := [][2]int{{5, 2}, {10, 1}, {7, 6}, {20, 1}, {15, 2}, {30, 3}, {17, 4}}
	var seeds []combinedSeed
	mi, ni := 0, 0
	for _, chunk := range pattern {
		for k := 0; k < chunk[0]; k++ {
			seeds = append(seeds, matches[mi])
			mi++
		}
		for k := 0; k < chunk[1]; k++ {
			seeds = append(seeds, nonMatches[ni])
			ni++
		}
	}
	return seeds
}

func twoDigits(n int) string {
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

// combinedWindowQuery builds the encoded query for account "carol" over the
// given window; limit 0 omits the parameter to exercise the default.
func combinedWindowQuery(from, to string, limit int64) string {
	values := url.Values{}
	values.Set("account", combinedAccount)
	values.Set("from", from)
	values.Set("to", to)
	if limit > 0 {
		values.Set("limit", strconv.FormatInt(limit, 10))
	}
	return values.Encode()
}

// filterWindow applies the public window semantics (from inclusive, to
// exclusive) to the predefined posts. from and to must be normalized UTC.
func filterWindow(posts []map[string]any, from, to string) []map[string]any {
	var out []map[string]any
	for _, post := range posts {
		at := post["occurred_at"].(string)
		if store.CompareInstants(at, from) >= 0 && store.CompareInstants(at, to) < 0 {
			out = append(out, post)
		}
	}
	return out
}

// checkCombinedFixture guards the input table itself: the match flags agree
// with the public semantics applied to the stored (normalized) text, and the
// occurred_at order genuinely differs from the append order.
func checkCombinedFixture(t *testing.T, seeds []combinedSeed, posts []map[string]any) {
	t.Helper()
	if len(seeds) != 123 {
		t.Fatalf("fixture: %d seeds, want 123", len(seeds))
	}
	descent := false
	for i, seed := range seeds {
		at := posts[i]["occurred_at"].(string)
		match := seed.account == combinedAccount &&
			store.CompareInstants(at, combinedFrom) >= 0 &&
			store.CompareInstants(at, combinedTo) < 0
		if match != seed.match {
			t.Fatalf("fixture: seed %d (%q, %q) match = %v, semantics give %v",
				i, seed.account, seed.occurredAt, seed.match, match)
		}
		if i > 0 && store.CompareInstants(at, posts[i-1]["occurred_at"].(string)) < 0 {
			descent = true
		}
	}
	if !descent {
		t.Fatal("fixture: occurred_at order must differ from append order")
	}
}

// wantCombinedTraversal pages through GET /events for the given window until
// the cursor closes, checking every page against the expected append
// responses: page sizes, full record fields, ascending seq, the filter
// conditions on every record, and a cursor that points at the page's last
// record until no further matches exist. limit 0 exercises the default page
// size; afterSeq seeds the first page's cursor.
func wantCombinedTraversal(t *testing.T, router http.Handler, from, to string, limit int64, afterSeq string, expected []map[string]any) {
	t.Helper()
	normFrom, ok := normalizeOccurredAt(from)
	if !ok {
		t.Fatalf("bad from %q", from)
	}
	normTo, ok := normalizeOccurredAt(to)
	if !ok {
		t.Fatalf("bad to %q", to)
	}
	effective := limit
	if effective == 0 {
		effective = defaultEventPageLimit
	}

	query := combinedWindowQuery(from, to, limit)
	if afterSeq != "" {
		query += "&after_seq=" + afterSeq
	}

	seen := 0
	var prevSeq float64
	for {
		recorder := getEventsQuery(t, router, query)
		if recorder.Code != http.StatusOK {
			t.Fatalf("query %q: status = %d (%s)", query, recorder.Code, recorder.Body.String())
		}
		events, next := decodePage(t, recorder)

		wantLen := int(effective)
		if remaining := len(expected) - seen; remaining < wantLen {
			wantLen = remaining
		}
		if len(events) != wantLen {
			t.Fatalf("query %q: page has %d events, want %d", query, len(events), wantLen)
		}
		for _, event := range events {
			posted := expected[seen]
			if len(event) != 8 {
				t.Fatalf("event keys = %v, want the 8 append response fields", event)
			}
			for _, key := range []string{"account", "operation", "resource", "result", "occurred_at", "seq", "prev_hash", "hash"} {
				if event[key] != posted[key] {
					t.Fatalf("event %s = %v, want the appended %v", key, event[key], posted[key])
				}
			}
			// Every returned record satisfies all filter conditions at once.
			if event["account"] != combinedAccount {
				t.Fatalf("event account = %v, want %q", event["account"], combinedAccount)
			}
			at := event["occurred_at"].(string)
			if store.CompareInstants(at, normFrom) < 0 || store.CompareInstants(at, normTo) >= 0 {
				t.Fatalf("event occurred_at %q outside [%q, %q)", at, normFrom, normTo)
			}
			seq := event["seq"].(float64)
			if seen > 0 && seq <= prevSeq {
				t.Fatalf("seq %v does not follow %v", seq, prevSeq)
			}
			prevSeq = seq
			seen++
		}

		if seen == len(expected) {
			// No further matches: the cursor closes even when non-matching
			// records still follow in the ledger.
			if next != nil {
				t.Fatalf("next_after_seq = %v after the last match, want null", next)
			}
			return
		}
		// More matches exist: the cursor points at this page's last record,
		// however many non-matching records lie before the next match.
		last := events[len(events)-1]["seq"]
		if next != last {
			t.Fatalf("next_after_seq = %v, want the page's last seq %v", next, last)
		}
		query = combinedWindowQuery(from, to, limit) + "&after_seq=" + strconv.FormatInt(int64(last.(float64)), 10)
	}
}

// wantErrorShape asserts the published error contract: the body is only a
// top-level error object with string code and message, and leaks nothing
// else — no partial page, SQL, stack trace or filesystem path.
func wantErrorShape(t *testing.T, recorder *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status = %d, want %d (%s)", recorder.Code, status, recorder.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if len(decoded) != 1 {
		t.Fatalf("error body keys = %v, want only the top-level error object", decoded)
	}
	errObj, ok := decoded["error"].(map[string]any)
	if !ok || len(errObj) != 2 {
		t.Fatalf("error = %v, want an object with only code and message", decoded["error"])
	}
	if errObj["code"] != code {
		t.Fatalf("code = %v, want %q", errObj["code"], code)
	}
	if message, ok := errObj["message"].(string); !ok || message == "" {
		t.Fatalf("message = %v, want a non-empty string", errObj["message"])
	}
	for _, leak := range []string{`"events"`, "next_after_seq", "SELECT", "goroutine", ".db"} {
		if strings.Contains(recorder.Body.String(), leak) {
			t.Fatalf("error body leaks %q: %s", leak, recorder.Body.String())
		}
	}
}

func TestGetEventsCombinedPaginationRegression(t *testing.T) {
	router := NewRouter(openTestStore(t))

	// Build the ledger through POST /events and keep every append response;
	// the expected pages below are derived from these predefined inputs.
	seeds := combinedSeeds()
	posts := make([]map[string]any, 0, len(seeds))
	for _, seed := range seeds {
		posts = append(posts, mustPost(t, router, eventBody(seed.account, seed.occurredAt)))
	}
	for i, post := range posts {
		if post["seq"] != float64(i+1) {
			t.Fatalf("append %d seq = %v, want %d", i, post["seq"], i+1)
		}
	}
	checkCombinedFixture(t, seeds, posts)

	var expected []map[string]any
	var carolPosts []map[string]any
	for i, seed := range seeds {
		if seed.account == combinedAccount {
			carolPosts = append(carolPosts, posts[i])
		}
		if seed.match {
			expected = append(expected, posts[i])
		}
	}
	if len(expected) != 104 {
		t.Fatalf("fixture: %d matching records, want 104", len(expected))
	}

	// Page sizes 1, the default 50 and the maximum 100 over 104 matches:
	// 104 pages, three pages (50, 50, 4) and two pages (100, 4).
	wantCombinedTraversal(t, router, combinedFrom, combinedTo, 1, "", expected)
	wantCombinedTraversal(t, router, combinedFrom, combinedTo, 0, "", expected)
	wantCombinedTraversal(t, router, combinedFrom, combinedTo, 100, "", expected)

	// Bounds spelled with a timezone offset name the same window and must
	// produce the same pages as the UTC spelling.
	wantCombinedTraversal(t, router, "2026-10-05T08:00:00+08:00", "2026-10-06T08:00:00+08:00", 100, "", expected)

	// Sub-windows sized by the sorted matching instants: exactly one full
	// default page closes the cursor, and a short page stays short.
	instants := make([]string, len(expected))
	for i, post := range expected {
		instants[i] = post["occurred_at"].(string)
	}
	sort.Slice(instants, func(a, b int) bool { return store.CompareInstants(instants[a], instants[b]) < 0 })
	for i := 1; i < len(instants); i++ {
		if store.CompareInstants(instants[i-1], instants[i]) == 0 {
			t.Fatalf("fixture: duplicate matching instant %q", instants[i])
		}
	}
	fullPage := filterWindow(expected, instants[0], instants[50])
	if len(fullPage) != 50 {
		t.Fatalf("fixture: sub-window has %d matches, want exactly one default page", len(fullPage))
	}
	wantCombinedTraversal(t, router, instants[0], instants[50], 0, "", fullPage)
	shortPage := filterWindow(expected, instants[0], instants[3])
	if len(shortPage) != 3 {
		t.Fatalf("fixture: sub-window has %d matches, want 3", len(shortPage))
	}
	wantCombinedTraversal(t, router, instants[0], instants[3], 0, "", shortPage)

	// A trailing-zero fraction names the same instant: the inclusive lower
	// bound keeps the record, the exclusive upper bound drops it, and the
	// stored fraction text keeps all four digits.
	zeroPost := filterWindow(expected, "2026-10-05T06:00:00.25Z", "2026-10-05T06:00:01Z")
	if len(zeroPost) != 1 {
		t.Fatalf("fixture: %d records at the trailing-zero instant, want 1", len(zeroPost))
	}
	wantCombinedTraversal(t, router, "2026-10-05T06:00:00.25Z", "2026-10-05T06:00:01Z", 0, "", zeroPost)
	if zeroPost[0]["occurred_at"] != "2026-10-05T06:00:00.2500Z" {
		t.Fatalf("stored fraction = %v, want the original .2500 text", zeroPost[0]["occurred_at"])
	}
	events, next := decodePage(t, getEventsQuery(t, router,
		combinedWindowQuery("2026-10-05T06:00:00Z", "2026-10-05T06:00:00.25Z", 0)))
	if len(events) != 0 || next != nil {
		t.Fatalf("events = %v next = %v, want the equal upper bound to exclude the record", events, next)
	}

	// A fraction longer than nanoseconds is stored verbatim and still
	// ordered exactly: the record sits inside its second, and an upper
	// bound equal to its own instant excludes it.
	fracPost := filterWindow(expected, "2026-10-05T12:00:00Z", "2026-10-05T12:00:01Z")
	if len(fracPost) != 1 {
		t.Fatalf("fixture: %d records in the fraction second, want 1", len(fracPost))
	}
	wantCombinedTraversal(t, router, "2026-10-05T12:00:00.000000000000001Z", "2026-10-05T12:00:01Z", 0, "", fracPost)
	if fracPost[0]["occurred_at"] != "2026-10-05T12:00:00.000000000000001Z" {
		t.Fatalf("stored fraction = %v, want the original 15-digit text", fracPost[0]["occurred_at"])
	}
	events, next = decodePage(t, getEventsQuery(t, router,
		combinedWindowQuery("2026-10-05T12:00:00Z", "2026-10-05T12:00:00.000000000000001Z", 0)))
	if len(events) != 0 || next != nil {
		t.Fatalf("events = %v next = %v, want the equal upper bound to exclude the record", events, next)
	}

	// The record exactly at the main window's to is excluded there, but a
	// window starting at that same instant includes it: from is inclusive.
	dayAfter := filterWindow(carolPosts, combinedTo, "2026-10-07T00:00:00Z")
	if len(dayAfter) != 3 {
		t.Fatalf("fixture: %d records on the next day, want 3", len(dayAfter))
	}
	wantCombinedTraversal(t, router, combinedTo, "2026-10-07T00:00:00Z", 0, "", dayAfter)

	// No matching account, no matching instant, and a cursor past the tail
	// all return an empty page and a null cursor.
	aliceQuery := url.Values{}
	aliceQuery.Set("account", "alice")
	aliceQuery.Set("from", combinedFrom)
	aliceQuery.Set("to", combinedTo)
	events, next = decodePage(t, getEventsQuery(t, router, aliceQuery.Encode()))
	if len(events) != 0 || next != nil {
		t.Fatalf("events = %v next = %v, want empty page and null cursor", events, next)
	}
	events, next = decodePage(t, getEventsQuery(t, router,
		combinedWindowQuery("2026-10-07T00:00:00Z", "2026-10-08T00:00:00Z", 0)))
	if len(events) != 0 || next != nil {
		t.Fatalf("events = %v next = %v, want empty page and null cursor", events, next)
	}
	events, next = decodePage(t, getEventsQuery(t, router,
		combinedWindowQuery(combinedFrom, combinedTo, 0)+"&after_seq="+strconv.Itoa(len(seeds))))
	if len(events) != 0 || next != nil {
		t.Fatalf("events = %v next = %v, want empty page and null cursor past the tail", events, next)
	}

	// A matching record appended between two page reads is visible to later
	// pages, while re-reading the already full first page yields identical
	// content.
	firstRecorder := getEventsQuery(t, router, combinedWindowQuery(combinedFrom, combinedTo, 0))
	firstEvents, firstNext := decodePage(t, firstRecorder)
	if len(firstEvents) != 50 || firstNext != expected[49]["seq"] {
		t.Fatalf("first page = %d events, cursor = %v; want 50 events ending at seq %v",
			len(firstEvents), firstNext, expected[49]["seq"])
	}
	extra := mustPost(t, router, eventBody(combinedAccount, "2026-10-05T20:00:00Z"))
	again := getEventsQuery(t, router, combinedWindowQuery(combinedFrom, combinedTo, 0))
	if again.Body.String() != firstRecorder.Body.String() {
		t.Fatalf("first page changed after an append:\n%s\n%s", firstRecorder.Body.String(), again.Body.String())
	}
	remainder := append(append([]map[string]any{}, expected[50:]...), extra)
	wantCombinedTraversal(t, router, combinedFrom, combinedTo, 0,
		strconv.FormatInt(int64(firstNext.(float64)), 10), remainder)

	// The reads never touched the chain: the whole ledger still verifies.
	total := len(seeds) + 1
	verifyRecorder := httptest.NewRecorder()
	router.ServeHTTP(verifyRecorder, httptest.NewRequest(http.MethodGet, "/ledger/verify", nil))
	if verifyRecorder.Code != http.StatusOK {
		t.Fatalf("verify: status = %d (%s)", verifyRecorder.Code, verifyRecorder.Body.String())
	}
	result := decodeEvent(t, verifyRecorder)
	if result["valid"] != true || result["checked"] != float64(total) || result["first_invalid_seq"] != nil {
		t.Fatalf("verify = %v, want a valid chain of %d records", result, total)
	}

	// Appends still continue at the tail, chained to the previous record.
	tail := mustPost(t, router, eventBody(combinedAccount, "2026-10-05T21:00:00Z"))
	if tail["seq"] != float64(total+1) || tail["prev_hash"] != extra["hash"] {
		t.Fatalf("tail append = %v, want seq %d chained to %v", tail, total+1, extra["hash"])
	}
	verifyRecorder = httptest.NewRecorder()
	router.ServeHTTP(verifyRecorder, httptest.NewRequest(http.MethodGet, "/ledger/verify", nil))
	result = decodeEvent(t, verifyRecorder)
	if result["valid"] != true || result["checked"] != float64(total+1) {
		t.Fatalf("verify = %v, want a valid chain of %d records", result, total+1)
	}
}

func TestGetEventsCombinedOnEmptyLedger(t *testing.T) {
	router := NewRouter(openTestStore(t))

	// Every parameter combined against an empty ledger: an empty array and
	// a null cursor, not an error.
	recorder := getEventsQuery(t, router, combinedWindowQuery(combinedFrom, combinedTo, 100)+"&after_seq=41")
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

func TestGetEventsCombinedRejectsInvalidInput(t *testing.T) {
	router := NewRouter(openTestStore(t))
	cases := []string{
		"account=carol&account=carol&from=2026-10-05T00:00:00Z&to=2026-10-06T00:00:00Z", // repeated parameter
		"account=carol&from=2026-10-05T00:00:00Z&to=2026-10-06T00:00:00Z&limit=1&limit=1",
		"account=carol&from=2026-10-06T00:00:00Z&to=2026-10-05T00:00:00Z",           // from after to
		"account=carol&from=2026-10-05T00:00:00Z&to=2026-10-05T00:00:00Z",           // empty window
		"account=carol&from=2026-10-05T08:00:00%2B08:00&to=2026-10-05T00:00:00Z",    // same instant
		"account=carol&from=2026-10-05T00:00:00Z&to=2026-10-06T00:00:00Z&limit=0",   // below range
		"account=carol&from=2026-10-05T00:00:00Z&to=2026-10-06T00:00:00Z&limit=101", // above range
		"account=carol&from=2026-10-05T00:00:00Z&to=2026-10-06T00:00:00Z&after_seq=-1",
		"account=carol&from=2026-10-05T00:00:00Z&to=2026-10-06T00:00:00Z&after_seq=9223372036854775808",
	}
	for _, query := range cases {
		wantErrorShape(t, getEventsQuery(t, router, query), http.StatusBadRequest, "invalid_audit_input")
	}
}

func TestGetEventsCombinedStorageUnavailable(t *testing.T) {
	st := openTestStore(t)
	router := NewRouter(st)
	st.Close()

	// Even with storage down, invalid parameters are rejected as 400 first.
	wantErrorShape(t, getEventsQuery(t, router,
		"account=carol&account=carol&from=2026-10-05T00:00:00Z&to=2026-10-06T00:00:00Z"),
		http.StatusBadRequest, "invalid_audit_input")
	wantErrorShape(t, getEventsQuery(t, router,
		"account=carol&from=2026-10-05T00:00:00Z&to=2026-10-06T00:00:00Z&limit=101"),
		http.StatusBadRequest, "invalid_audit_input")

	// A fully valid combined request surfaces the storage failure as 503.
	wantErrorShape(t, getEventsQuery(t, router,
		combinedWindowQuery(combinedFrom, combinedTo, 100)+"&after_seq=10"),
		http.StatusServiceUnavailable, "storage_unavailable")
}

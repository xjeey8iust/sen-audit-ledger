package api

// Combined-pagination regression tests for GET /events.
//
// The single-purpose tests in list_test.go pin each filter and pagination
// rule on its own; this file exercises them combined against one large,
// predetermined ledger:
//
//   - 255 records, so page sizes 1, 50 and 100 all split matches across
//     non-adjacent seq ranges (the no-filter traversal at limit 100 spans
//     three pages, and at the default limit six);
//   - 120 "alice" records interleaved one-for-one with case-variant,
//     whitespace-variant and other-account rows;
//   - the alice occurrence times follow a fixed permutation of the 120
//     minutes 01:00..02:59, so occurrence order deliberately differs from
//     append (seq) order;
//   - a fixed boundary block around 03:00/04:00 UTC with sub-nanosecond
//     fractions, trailing-zero equivalents and an offset-spelled input;
//   - a record appended between two page requests;
//   - the whole-ledger verification and tail continuation afterwards;
//   - the 400 invalid_audit_input / 503 storage_unavailable error
//     envelopes, including parameter-vs-storage precedence.
//
// Every expectation below (which seqs match a query, how a page splits, what
// the next cursor is) is computed from the fixed input plan plus the public
// README ordering semantics (store.CompareInstants), never from the query
// under test. Each read-back record is compared field-for-field with the
// 201 response captured for its seq.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/xjeey8iust/sen-audit-ledger/internal/store"
)

const (
	comboTotalRows = 255 // fixed ledger size
	comboMainFrom  = "2026-10-05T01:00:00Z"
	comboMainTo    = "2026-10-05T03:00:00Z"
	comboDayFrom   = "2026-10-05T00:00:00Z"
	comboDayTo     = "2026-10-06T00:00:00Z"
)

// comboWindow is one occurrence-time window in normalized UTC spelling
// ("" means the bound is omitted).
type comboWindow struct {
	from string
	to   string
}

// comboSeedSpec describes one row of the fixed ledger before posting. The
// seq of each entry is its slice index plus one. occurredAt is the exact
// text posted; occurredUTC is the same instant's normalized UTC form used
// to derive expectations (the response must store occurredUTC).
type comboSeedSpec struct {
	account     string
	occurredAt  string
	occurredUTC string
}

// comboUTC builds a spec whose posted text is already normalized UTC.
func comboUTC(account, at string) comboSeedSpec {
	return comboSeedSpec{account: account, occurredAt: at, occurredUTC: at}
}

// comboOffset builds a spec posted with a timezone offset and records the
// UTC instant it denotes, so expectations compare instants, not spellings.
func comboOffset(account, posted, utc string) comboSeedSpec {
	return comboSeedSpec{account: account, occurredAt: posted, occurredUTC: utc}
}

// comboLedgerPlan builds the deterministic 255-row ledger used by the
// combined tests. Rows are posted in the returned order, so seq order is
// this order, while occurrence times jump around on purpose.
//
// Seqs 1..240 alternate "alice" with non-matching rows. The 120 alice rows
// carry the 120 minutes 01:00:00..02:59:00 in a fixed coprime permutation
// (minute = (j*37+11) mod 120), so time order and append order differ.
// The interleaved rows sit at 05:00 and therefore never fall inside the
// 01:00..03:00 windows, while exercising case and whitespace sensitivity.
//
// Seqs 241..255 are the fixed boundary block around 03:00/04:00. The block
// window [03:00,04:00) holds the eight alice rows listed in comboBlockSeqs;
// note that seqs 246 and 250 (02:59 alice) sit just below the block and
// therefore belong to the main 01:00..03:00 window instead.
func comboLedgerPlan() []comboSeedSpec {
	specs := make([]comboSeedSpec, 0, comboTotalRows)
	for i := 0; i < 240; i++ {
		if i%2 == 0 {
			j := i / 2 // 0..119
			minute := (j*37 + 11) % 120
			specs = append(specs, comboUTC("alice",
				fmt.Sprintf("2026-10-05T%02d:%02d:00Z", 1+minute/60, minute%60)))
			continue
		}
		// A non-matching row after every alice row.
		var account string
		switch (i / 2) % 4 {
		case 0:
			account = "Alice"
		case 1:
			account = "ALICE"
		case 2:
			account = " alice "
		default:
			account = "bob"
		}
		specs = append(specs, comboUTC(account, "2026-10-05T05:00:00Z"))
	}

	// Fixed boundary block: seqs 241..255, all on 2026-10-05 UTC. The block
	// window is [03:00:00Z, 04:00:00Z); comments pin each row's role.
	specs = append(specs,
		comboUTC("alice", "2026-10-05T03:00:00Z"),                                 // 241, on the inclusive lower boundary
		comboUTC("bob", "2026-10-05T03:00:00Z"),                                   // 242, other account
		comboUTC("Alice", "2026-10-05T03:00:00Z"),                                 // 243, case variant
		comboUTC(" alice ", "2026-10-05T03:00:00Z"),                               // 244, whitespace variant
		comboUTC("alice", "2026-10-05T03:00:00.000000000000001Z"),                 // 245, 15 fractional digits
		comboUTC("alice", "2026-10-05T02:59:59.999999999999999Z"),                 // 246, one padded tick before the boundary
		comboUTC("alice", "2026-10-05T03:00:00.5Z"),                               // 247
		comboUTC("alice", "2026-10-05T03:00:00.500000000000000Z"),                 // 248, same instant as 247, longer text
		comboUTC("alice", "2026-10-05T03:00:00.50Z"),                              // 249, same instant, trailing-zero text
		comboUTC("alice", "2026-10-05T02:59:59Z"),                                 // 250, one whole second below the boundary
		comboUTC("alice", "2026-10-05T03:00:00.5001Z"),                            // 251, strictly after the .5 instant
		comboOffset("alice", "2026-10-05T11:30:00+08:00", "2026-10-05T03:30:00Z"), // 252, offset spelling
		comboUTC("carol", "2026-10-05T03:30:00Z"),                                 // 253, non-matching gap between alice rows
		comboUTC("alice", "2026-10-05T03:59:59.999Z"),                             // 254, one ms before the upper boundary
		comboUTC("alice", "2026-10-05T04:00:00Z"),                                 // 255, on the excluded upper boundary
	)
	return specs
}

// comboBlockSeqs is the predetermined alice seq set inside the block window
// [03:00:00Z, 04:00:00Z). It is the human-checked expectation the computed
// plan is asserted against.
var comboBlockSeqs = []int64{241, 245, 247, 248, 249, 251, 252, 254}

// comboPlan is the lazily-built shared plan (constant after init).
func comboPlan() []comboSeedSpec { return comboLedgerPlan() }

// seedComboLedger posts the whole plan in order and returns the captured
// 201 response of every seq, indexed by seq-1.
func seedComboLedger(t *testing.T, router http.Handler) []map[string]any {
	t.Helper()
	specs := comboPlan()
	if len(specs) != comboTotalRows {
		t.Fatalf("plan size = %d, want %d", len(specs), comboTotalRows)
	}
	posted := make([]map[string]any, len(specs))
	for i, spec := range specs {
		recorder := postEventBody(t, router, eventBody(spec.account, spec.occurredAt))
		if recorder.Code != http.StatusCreated {
			t.Fatalf("seed seq %d: status = %d (%s)", i+1, recorder.Code, recorder.Body.String())
		}
		posted[i] = decodeEvent(t, recorder)
		if posted[i]["seq"] != float64(i+1) {
			t.Fatalf("seed seq %d: response seq = %v", i+1, posted[i]["seq"])
		}
		// The stored time is the normalized UTC instant of the posted text.
		if posted[i]["occurred_at"] != spec.occurredUTC {
			t.Fatalf("seed seq %d: occurred_at = %v, want normalized %v",
				i+1, posted[i]["occurred_at"], spec.occurredUTC)
		}
	}
	return posted
}

// comboExpectSeqs independently computes the seqs matching an account and
// window from the fixed plan and the public instant-ordering rule. A nil
// account and empty bounds apply no constraint. It never queries the
// service, so it is a genuine expectation rather than a self-fulfilling one.
func comboExpectSeqs(account *string, window comboWindow, afterSeq int64) []int64 {
	specs := comboPlan()
	// Bounds denote instants: normalize offset spellings to UTC exactly as
	// the query endpoint does, so expectations compare actual instants
	// rather than text.
	from, to := window.from, window.to
	if from != "" {
		normalized, ok := normalizeOccurredAt(from)
		if !ok {
			panic("test window from is not a valid timestamp: " + from)
		}
		from = normalized
	}
	if to != "" {
		normalized, ok := normalizeOccurredAt(to)
		if !ok {
			panic("test window to is not a valid timestamp: " + to)
		}
		to = normalized
	}
	var matches []int64
	for i, spec := range specs {
		seq := int64(i + 1)
		if seq <= afterSeq {
			continue
		}
		if account != nil && spec.account != *account {
			continue
		}
		if from != "" && store.CompareInstants(spec.occurredUTC, from) < 0 {
			continue
		}
		if to != "" && store.CompareInstants(spec.occurredUTC, to) >= 0 {
			continue
		}
		matches = append(matches, seq)
	}
	return matches
}

// comboBuildQuery renders one GET /events query from combined parameters;
// empty account/from/to omit that parameter, limit 0 omits limit.
func comboBuildQuery(account string, window comboWindow, limit int, afterSeq int64) string {
	values := url.Values{}
	if account != "" {
		values.Set("account", account)
	}
	if window.from != "" {
		values.Set("from", window.from)
	}
	if window.to != "" {
		values.Set("to", window.to)
	}
	if limit != 0 {
		values.Set("limit", fmt.Sprintf("%d", limit))
	}
	if afterSeq != 0 {
		values.Set("after_seq", fmt.Sprintf("%d", afterSeq))
	}
	return values.Encode()
}

// comboEffectiveLimit maps the omitted limit to the published default.
func comboEffectiveLimit(limit int) int64 {
	if limit == 0 {
		return defaultEventPageLimit
	}
	return int64(limit)
}

// comboFetchPage issues one GET /events request and requires HTTP 200.
func comboFetchPage(t *testing.T, router http.Handler, rawQuery string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := getEventsQuery(t, router, rawQuery)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /events?%s: status = %d (%s)", rawQuery, recorder.Code, recorder.Body.String())
	}
	return recorder
}

// comboAssertRecordFields fails unless one page item equals the captured 201
// response of its seq on every published field and carries no extra keys.
func comboAssertRecordFields(t *testing.T, item map[string]any, posted map[string]any) {
	t.Helper()
	if len(item) != 8 {
		t.Fatalf("event keys = %v, want the 8 append response fields", item)
	}
	for _, key := range []string{"account", "operation", "resource", "result", "occurred_at", "seq", "prev_hash", "hash"} {
		if item[key] != posted[key] {
			t.Fatalf("seq %v field %s = %v, want %v from the append response", item["seq"], key, item[key], posted[key])
		}
	}
}

// comboPageCase describes one traversal and its predetermined result.
type comboPageCase struct {
	name     string
	account  string // "" applies no account constraint
	window   comboWindow
	limit    int // 0 means the published default
	afterSeq int64
	wantSeqs []int64
}

// comboWalkPages follows next_after_seq from the case's start cursor until
// it closes, returning every matched record and every emitted cursor.
func comboWalkPages(t *testing.T, router http.Handler, tc comboPageCase) ([]map[string]any, []int64) {
	t.Helper()
	effLimit := comboEffectiveLimit(tc.limit)
	var collected []map[string]any
	var cursors []int64
	cursor := tc.afterSeq
	for pages := 0; ; pages++ {
		if pages > len(tc.wantSeqs)+1 {
			t.Fatalf("%s: pagination did not close after %d pages", tc.name, pages)
		}
		recorder := comboFetchPage(t, router, comboBuildQuery(tc.account, tc.window, tc.limit, cursor))
		events, next := decodePage(t, recorder)
		if int64(len(events)) > effLimit {
			t.Fatalf("%s: page %d has %d events, limit %d", tc.name, pages, len(events), effLimit)
		}
		for i, item := range events {
			seq := int64(item["seq"].(float64))
			if i > 0 {
				prev := int64(events[i-1]["seq"].(float64))
				if seq <= prev {
					t.Fatalf("%s: page %d seqs not strictly increasing: %d after %d", tc.name, pages, seq, prev)
				}
			}
			if cursor != 0 && seq <= cursor {
				t.Fatalf("%s: page returned seq %d at or after cursor %d", tc.name, seq, cursor)
			}
			collected = append(collected, item)
		}
		if next == nil {
			return collected, cursors
		}
		nextSeq, ok := next.(float64)
		if !ok || nextSeq <= 0 {
			t.Fatalf("%s: next_after_seq = %v, want a positive integer", tc.name, next)
		}
		if len(events) == 0 {
			t.Fatalf("%s: non-null cursor %v on an empty page", tc.name, nextSeq)
		}
		last := int64(events[len(events)-1]["seq"].(float64))
		if int64(nextSeq) != last {
			t.Fatalf("%s: next_after_seq = %d, want the page's last seq %d (non-matching rows do not move the cursor)",
				tc.name, int64(nextSeq), last)
		}
		cursors = append(cursors, int64(nextSeq))
		cursor = int64(nextSeq)
	}
}

// comboRunPageCase walks one case and verifies the complete traversal
// against the predetermined expectation: the exact seq set with no
// omissions or duplicates, strictly increasing pages, full fields per
// record, every returned record independently satisfying all combined
// conditions, and a cursor on every page except the last.
func comboRunPageCase(t *testing.T, router http.Handler, posted []map[string]any, tc comboPageCase) {
	t.Helper()
	collected, cursors := comboWalkPages(t, router, tc)

	gotSeqs := make([]int64, 0, len(collected))
	seen := map[int64]bool{}
	for _, item := range collected {
		seq := int64(item["seq"].(float64))
		if seen[seq] {
			t.Fatalf("%s: seq %d returned twice across pages", tc.name, seq)
		}
		seen[seq] = true
		gotSeqs = append(gotSeqs, seq)

		// Full-field equality with this seq's append response.
		comboAssertRecordFields(t, item, posted[seq-1])

		// Independently re-check every combined condition against the
		// returned values, rather than trusting the query that selected it.
		if tc.account != "" && item["account"] != tc.account {
			t.Fatalf("%s: seq %d account = %v, want %q", tc.name, seq, item["account"], tc.account)
		}
		occurredAt := item["occurred_at"].(string)
		if tc.window.from != "" && store.CompareInstants(occurredAt, tc.window.from) < 0 {
			t.Fatalf("%s: seq %d occurred_at %s is before from %s", tc.name, seq, occurredAt, tc.window.from)
		}
		if tc.window.to != "" && store.CompareInstants(occurredAt, tc.window.to) >= 0 {
			t.Fatalf("%s: seq %d occurred_at %s is not before to %s", tc.name, seq, occurredAt, tc.window.to)
		}
		if seq <= tc.afterSeq {
			t.Fatalf("%s: seq %d does not respect after_seq %d", tc.name, seq, tc.afterSeq)
		}
	}

	want := append([]int64(nil), tc.wantSeqs...)
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
	if len(gotSeqs) != len(want) {
		t.Fatalf("%s: collected %d seqs %v, want %d seqs %v", tc.name, len(gotSeqs), gotSeqs, len(want), want)
	}
	for i := range want {
		if gotSeqs[i] != want[i] {
			t.Fatalf("%s: collected seqs %v, want %v", tc.name, gotSeqs, want)
		}
	}
	// A non-null cursor is emitted on every page except the last one.
	wantCursors := 0
	if n := len(want); n > 0 {
		wantCursors = (n - 1) / int(comboEffectiveLimit(tc.limit))
	}
	if len(cursors) != wantCursors {
		t.Fatalf("%s: emitted %d non-null cursors, want %d", tc.name, len(cursors), wantCursors)
	}
}

// comboAliceExpect builds the expectation for an alice window case.
func comboAliceExpect(window comboWindow, afterSeq int64) []int64 {
	account := "alice"
	return comboExpectSeqs(&account, window, afterSeq)
}

// comboPlanSanityChecks fails early if the fixed input plan does not have
// the properties the test matrix relies on.
func comboPlanSanityChecks(t *testing.T) {
	t.Helper()
	specs := comboPlan()

	// The boundary block expectation matches the human-checked seq list.
	block := comboWindow{from: "2026-10-05T03:00:00Z", to: "2026-10-05T04:00:00Z"}
	if got := comboAliceExpect(block, 0); fmt.Sprint(got) != fmt.Sprint(comboBlockSeqs) {
		t.Fatalf("block plan seqs = %v, want %v", got, comboBlockSeqs)
	}

	// The main [01:00,03:00) window holds the 120 minute rows plus the two
	// 02:59 alice rows from the boundary block = 122 matches; specially
	// chosen sub-windows hold exactly 100, 51 and 50 of them.
	mainWindow := comboWindow{from: comboMainFrom, to: comboMainTo}
	check := func(label string, window comboWindow, want int) {
		got := len(comboAliceExpect(window, 0))
		if got != want {
			t.Fatalf("%s: plan yields %d alice matches, want %d", label, got, want)
		}
	}
	check("main window", mainWindow, 122)
	check("exact 100 window", comboWindow{from: "2026-10-05T01:22:00Z", to: comboMainTo}, 100)
	check("exact 51 window", comboWindow{from: "2026-10-05T02:11:00Z", to: comboMainTo}, 51)
	check("exact 50 window", comboWindow{from: "2026-10-05T02:12:00Z", to: comboMainTo}, 50)
	check("single match window", comboWindow{from: comboMainFrom, to: "2026-10-05T01:01:00Z"}, 1)

	// Occurrence order really differs from append order among alice rows.
	aliceMain := comboAliceExpect(mainWindow, 0)
	if len(aliceMain) < 2 {
		t.Fatalf("need at least two main-window alice rows, got %d", len(aliceMain))
	}
	timesInSeqOrder := make([]string, len(aliceMain))
	for i, seq := range aliceMain {
		timesInSeqOrder[i] = specs[seq-1].occurredUTC
	}
	monotonic := sort.StringsAreSorted(timesInSeqOrder)
	if monotonic {
		t.Fatalf("alice occurrence times must not follow append order: %v", timesInSeqOrder[:5])
	}
}

// TestGetEventsCombinedPaginationMatrix pages the fixed ledger through the
// required page sizes (1, default 50, max 100) against match counts below
// one page, exactly one page, one page plus one, and more than two pages,
// with account, window and cursor all combined and non-matching rows
// interleaved throughout.
func TestGetEventsCombinedPaginationMatrix(t *testing.T) {
	comboPlanSanityChecks(t)
	router := NewRouter(openTestStore(t))
	posted := seedComboLedger(t, router)

	day := comboWindow{from: comboDayFrom, to: comboDayTo}
	mainWindow := comboWindow{from: comboMainFrom, to: comboMainTo}
	exact100 := comboWindow{from: "2026-10-05T01:22:00Z", to: comboMainTo}
	exact51 := comboWindow{from: "2026-10-05T02:11:00Z", to: comboMainTo}
	exact50 := comboWindow{from: "2026-10-05T02:12:00Z", to: comboMainTo}
	single := comboWindow{from: comboMainFrom, to: "2026-10-05T01:01:00Z"}
	block := comboWindow{from: "2026-10-05T03:00:00Z", to: "2026-10-05T04:00:00Z"}

	allDay := comboExpectSeqs(nil, day, 0)
	aliceDay := comboAliceExpect(day, 0)

	cases := []comboPageCase{
		// No account filter: all 255 rows. More than two pages at size 100,
		// six pages at the default size, one record per page at size 1.
		{name: "all rows limit 100", window: day, limit: 100, wantSeqs: allDay},
		{name: "all rows default limit", window: day, limit: 0, wantSeqs: allDay},
		{name: "all rows limit 1", window: day, limit: 1, wantSeqs: allDay},

		// Account + day window: the 122 main-window alice plus the 8 block
		// alice rows; matches skip every interleaved non-matching row.
		{name: "alice day limit 100", account: "alice", window: day, limit: 100, wantSeqs: aliceDay},
		{name: "alice day default limit", account: "alice", window: day, limit: 0, wantSeqs: aliceDay},
		{name: "alice day limit 50 three pages", account: "alice", window: day, limit: 50, wantSeqs: aliceDay},
		{name: "alice day limit 1", account: "alice", window: day, limit: 1, wantSeqs: aliceDay},

		// Exactly one full page must close the cursor, even though many rows
		// exist afterwards for other windows/accounts.
		{name: "alice exactly 100 fills max page", account: "alice", window: exact100, limit: 100,
			wantSeqs: comboAliceExpect(exact100, 0)},
		{name: "alice exactly 50 fills default page", account: "alice", window: exact50, limit: 50,
			wantSeqs: comboAliceExpect(exact50, 0)},
		{name: "alice exactly 1 fills size-1 page", account: "alice", window: single, limit: 1,
			wantSeqs: comboAliceExpect(single, 0)},

		// One page plus one record: a full page and a short tail page.
		{name: "alice 51 at limit 50", account: "alice", window: exact51, limit: 50,
			wantSeqs: comboAliceExpect(exact51, 0)},
		{name: "alice 130 at limit 100", account: "alice", window: day, limit: 100, wantSeqs: aliceDay},

		// Below one page for the two big page sizes.
		{name: "block 8 below limit 100", account: "alice", window: block, limit: 100,
			wantSeqs: comboBlockSeqs},
		{name: "block 8 below limit 50", account: "alice", window: block, limit: 50,
			wantSeqs: comboBlockSeqs},

		// The 122-match main window at each page size, with non-matching
		// rows interleaved.
		{name: "alice main limit 100", account: "alice", window: mainWindow, limit: 100,
			wantSeqs: comboAliceExpect(mainWindow, 0)},
		{name: "alice main default", account: "alice", window: mainWindow, limit: 0,
			wantSeqs: comboAliceExpect(mainWindow, 0)},
		{name: "alice main limit 1", account: "alice", window: mainWindow, limit: 1,
			wantSeqs: comboAliceExpect(mainWindow, 0)},

		// Combined with a start cursor that skips the first chunk of
		// matches, including at the odd boundary inside a block.
		{name: "alice day limit 50 after cursor 100", account: "alice", window: day, limit: 50,
			afterSeq: 100, wantSeqs: comboAliceExpect(day, 100)},
		{name: "alice main limit 1 after cursor 7", account: "alice", window: mainWindow, limit: 1,
			afterSeq: 7, wantSeqs: comboAliceExpect(mainWindow, 7)},
		{name: "alice block limit 1 after cursor 245", account: "alice", window: block, limit: 1,
			afterSeq: 245, wantSeqs: comboAliceExpect(block, 245)},

		// Case and whitespace variants are distinct accounts.
		{name: "Alice case variant", account: "Alice", window: day, limit: 100,
			wantSeqs: comboExpectSeqs(ptru("Alice"), day, 0)},
		{name: "ALICE case variant", account: "ALICE", window: day, limit: 100,
			wantSeqs: comboExpectSeqs(ptru("ALICE"), day, 0)},
		{name: "whitespace variant", account: " alice ", window: day, limit: 100,
			wantSeqs: comboExpectSeqs(ptru(" alice "), day, 0)},
		{name: "double whitespace variant", account: "  alice  ", window: day, limit: 100,
			wantSeqs: comboExpectSeqs(ptru("  alice  "), day, 0)},
		{name: "bob", account: "bob", window: day, limit: 100,
			wantSeqs: comboExpectSeqs(ptru("bob"), day, 0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			comboRunPageCase(t, router, posted, tc)
		})
	}
}

// ptru returns a pointer to s, for the optional account expectation.
func ptru(s string) *string { return &s }

// TestGetEventsCombinedExactFullPageClosesCursor pins the rule that a page
// filled exactly to the limit closes next_after_seq when no further MATCH
// exists, however many non-matching rows follow in the ledger.
func TestGetEventsCombinedExactFullPageClosesCursor(t *testing.T) {
	router := NewRouter(openTestStore(t))
	posted := seedComboLedger(t, router)

	exact100 := comboWindow{from: "2026-10-05T01:22:00Z", to: comboMainTo}
	want100 := comboAliceExpect(exact100, 0)
	recorder := comboFetchPage(t, router, comboBuildQuery("alice", exact100, 100, 0))
	events, next := decodePage(t, recorder)
	if len(events) != 100 || next != nil {
		t.Fatalf("exact-100 page: %d events, next = %v; want 100 and null", len(events), next)
	}
	for i, item := range events {
		comboAssertRecordFields(t, item, posted[want100[i]-1])
	}

	exact50 := comboWindow{from: "2026-10-05T02:12:00Z", to: comboMainTo}
	want50 := comboAliceExpect(exact50, 0)
	recorder = comboFetchPage(t, router, comboBuildQuery("alice", exact50, 50, 0))
	events, next = decodePage(t, recorder)
	if len(events) != 50 || next != nil {
		t.Fatalf("exact-50 page: %d events, next = %v; want 50 and null", len(events), next)
	}
	for i, item := range events {
		comboAssertRecordFields(t, item, posted[want50[i]-1])
	}
}

// TestGetEventsCombinedTimeBoundaries pins from-inclusive/to-exclusive
// semantics with sub-nanosecond fractions, trailing-zero equivalents and
// timezone-offset bounds, and checks the stored fractional text survives.
func TestGetEventsCombinedTimeBoundaries(t *testing.T) {
	router := NewRouter(openTestStore(t))
	posted := seedComboLedger(t, router)

	// The same block bounds spelled with timezone offsets ('+' encoded):
	// 11:00/12:00 in +08:00 name 03:00/04:00 UTC.
	offsetQuery := "account=alice&from=" + url.QueryEscape("2026-10-05T11:00:00+08:00") +
		"&to=" + url.QueryEscape("2026-10-05T12:00:00+08:00") + "&limit=100"
	recorder := comboFetchPage(t, router, offsetQuery)
	events, next := decodePage(t, recorder)
	comboAssertSeqList(t, "offset block", events, comboBlockSeqs)
	if next != nil {
		t.Fatalf("offset block: next_after_seq = %v, want null", next)
	}
	// The offset-spelled stored record (seq 252) reads back in UTC.
	if posted[251]["occurred_at"] != "2026-10-05T03:30:00Z" {
		t.Fatalf("seq 252 occurred_at = %v, want normalized 03:30:00Z", posted[251]["occurred_at"])
	}

	cases := []struct {
		name   string
		window comboWindow
		want   []int64
	}{
		// Lower bound one padded tick below 03:00 pulls in seq 246.
		{"from one sub-nanosecond tick below",
			comboWindow{from: "2026-10-05T02:59:59.999999999999999Z", to: "2026-10-05T04:00:00Z"},
			[]int64{241, 245, 246, 247, 248, 249, 251, 252, 254}},
		// A lower bound exactly on the 15-digit instant includes seq 245
		// but excludes seq 241, which sits one tick earlier.
		{"from equals sub-nanosecond record",
			comboWindow{from: "2026-10-05T03:00:00.000000000000001Z", to: "2026-10-05T04:00:00Z"},
			[]int64{245, 247, 248, 249, 251, 252, 254}},
		// One padded tick later excludes it.
		{"from one tick after sub-nanosecond record",
			comboWindow{from: "2026-10-05T03:00:00.000000000000002Z", to: "2026-10-05T04:00:00Z"},
			[]int64{247, 248, 249, 251, 252, 254}},
		// ".50" names the same instant as ".5": equal as a lower bound...
		{"from .50 equals .5",
			comboWindow{from: "2026-10-05T03:00:00.50Z", to: "2026-10-05T04:00:00Z"},
			[]int64{247, 248, 249, 251, 252, 254}},
		// ...and the same value excludes the .5 records as an upper bound.
		{"to .50 excludes .5 records",
			comboWindow{from: "2026-10-05T03:00:00Z", to: "2026-10-05T03:00:00.50Z"},
			[]int64{241, 245}},
		// [from, .5001) keeps the record strictly after .5 but excludes the
		// record sitting exactly on the upper bound.
		{"to .5001 excludes its own record",
			comboWindow{from: "2026-10-05T03:00:00Z", to: "2026-10-05T03:00:00.5001Z"},
			[]int64{241, 245, 247, 248, 249}},
		// The upper boundary spelled with a positive offset is the same
		// excluded instant: seq 255 stays out, seq 254 stays in.
		{"to via offset excludes boundary",
			comboWindow{from: "2026-10-05T03:00:00Z", to: "2026-10-05T13:00:00+09:00"},
			comboBlockSeqs},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Every case must agree with the independently computed plan.
			if got := comboAliceExpect(tc.window, 0); fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("plan-derived expectation %v, want %v", got, tc.want)
			}
			comboRunPageCase(t, router, posted, comboPageCase{
				name:     tc.name,
				account:  "alice",
				window:   tc.window,
				limit:    100,
				wantSeqs: tc.want,
			})
		})
	}

	// Full-field read-back of the fraction-sensitive rows, including the
	// verbatim fractional text captured in the append response.
	day := comboWindow{from: comboDayFrom, to: comboDayTo}
	for _, seq := range []int64{245, 246, 247, 248, 249, 251, 254, 255} {
		item := comboGetOne(t, router, day, seq)
		comboAssertRecordFields(t, item, posted[seq-1])
	}
	if posted[244]["occurred_at"] != "2026-10-05T03:00:00.000000000000001Z" {
		t.Fatalf("seq 245 fraction text not preserved: %v", posted[244]["occurred_at"])
	}
	if posted[248]["occurred_at"] != "2026-10-05T03:00:00.50Z" {
		t.Fatalf("seq 249 trailing-zero text not preserved: %v", posted[248]["occurred_at"])
	}
}

// comboGetOne reads one stored record by seq through the API (using a
// window wide enough to contain it), following the cursor as needed, and
// fails if the seq is absent.
func comboGetOne(t *testing.T, router http.Handler, window comboWindow, seq int64) map[string]any {
	t.Helper()
	cursor := int64(0)
	for {
		recorder := comboFetchPage(t, router, comboBuildQuery("alice", window, 100, cursor))
		events, next := decodePage(t, recorder)
		for _, item := range events {
			if int64(item["seq"].(float64)) == seq {
				return item
			}
		}
		if next == nil {
			t.Fatalf("seq %d not found via the API", seq)
		}
		cursor = int64(next.(float64))
	}
}

// comboAssertSeqList fails unless a page contains exactly the given seqs
// in ascending order.
func comboAssertSeqList(t *testing.T, label string, events []map[string]any, want []int64) {
	t.Helper()
	if len(events) != len(want) {
		t.Fatalf("%s: %d events, want seqs %v", label, len(events), want)
	}
	for i, seq := range want {
		if got := int64(events[i]["seq"].(float64)); got != seq {
			t.Fatalf("%s: event %d seq = %d, want %d", label, i, got, seq)
		}
	}
}

// TestGetEventsCombinedCursorSkipsNonMatchingRows focuses on the rule that
// next_after_seq always names the page's last matched seq even when many
// non-matching rows follow, and closes only once no later match exists.
func TestGetEventsCombinedCursorSkipsNonMatchingRows(t *testing.T) {
	router := NewRouter(openTestStore(t))
	posted := seedComboLedger(t, router)
	block := comboWindow{from: "2026-10-05T03:00:00Z", to: "2026-10-05T04:00:00Z"}

	// limit 1 from cursor 241 walks the block matches one at a time; each
	// cursor names the last match although non-matching rows (242-244, 246,
	// 250, 253) and excluded matches (246, 250, 255) intervene.
	cursor := int64(241)
	for _, want := range comboBlockSeqs[1:] {
		recorder := comboFetchPage(t, router, comboBuildQuery("alice", block, 1, cursor))
		events, next := decodePage(t, recorder)
		comboAssertSeqList(t, "block walk", events, []int64{want})
		if want == 254 {
			if next != nil {
				t.Fatalf("after seq 254: next_after_seq = %v, want null (255 sits on the excluded boundary)", next)
			}
		} else {
			if next != float64(want) {
				t.Fatalf("after seq %d: next_after_seq = %v, want %d", cursor, next, want)
			}
		}
		comboAssertRecordFields(t, events[0], posted[want-1])
		cursor = want
	}

	// A cursor already at the last match returns an empty page and null.
	recorder := comboFetchPage(t, router, comboBuildQuery("alice", block, 1, 254))
	events, next := decodePage(t, recorder)
	if len(events) != 0 || next != nil {
		t.Fatalf("past last block match: %v / %v, want empty page and null", events, next)
	}

	// A size-5 page straddling non-matching gaps inside the block.
	recorder = comboFetchPage(t, router, comboBuildQuery("alice", block, 5, 245))
	events, next = decodePage(t, recorder)
	comboAssertSeqList(t, "gap straddle", events, []int64{247, 248, 249, 251, 252})
	if next != float64(252) {
		t.Fatalf("gap straddle cursor = %v, want the page-end seq 252", next)
	}
	// The short final page closes the cursor even though rows 253 (carol)
	// and 255 (excluded alice) follow in the ledger.
	recorder = comboFetchPage(t, router, comboBuildQuery("alice", block, 5, 252))
	events, next = decodePage(t, recorder)
	comboAssertSeqList(t, "block tail", events, []int64{254})
	if next != nil {
		t.Fatalf("block tail cursor = %v, want null", next)
	}

	// In the day window a full first page points at its own last matched
	// seq even though plenty of rows follow, and the next page resumes
	// strictly after it without losing or repeating anything.
	day := comboWindow{from: comboDayFrom, to: comboDayTo}
	recorder = comboFetchPage(t, router, comboBuildQuery("alice", day, 100, 0))
	events, next = decodePage(t, recorder)
	if len(events) != 100 {
		t.Fatalf("first 100-page: %d events", len(events))
	}
	last := int64(events[99]["seq"].(float64))
	if next != float64(last) {
		t.Fatalf("next_after_seq = %v, want the page's last seq %d", next, last)
	}
	wantTail := comboAliceExpect(day, last)
	recorder = comboFetchPage(t, router, comboBuildQuery("alice", day, 100, last))
	events, next = decodePage(t, recorder)
	comboAssertSeqList(t, "alice tail", events, wantTail)
	if next != nil {
		t.Fatalf("alice tail cursor = %v, want null", next)
	}
}

// TestGetEventsCombinedEmptyLedgerNoMatchPastTail covers the three empty
// shapes: an empty ledger, a filter that matches nothing, and a cursor past
// the tail. Each returns an empty array and a null cursor.
func TestGetEventsCombinedEmptyLedgerNoMatchPastTail(t *testing.T) {
	empty := NewRouter(openTestStore(t))
	for _, query := range []string{
		"",
		"account=alice",
		"from=2026-10-05T00:00:00Z&to=2026-10-05T06:00:00Z",
		"limit=1",
		"after_seq=1",
		"account=alice&from=2026-10-05T00:00:00Z&to=2026-10-05T06:00:00Z&limit=100&after_seq=999",
	} {
		recorder := comboFetchPage(t, empty, query)
		events, next := decodePage(t, recorder)
		if len(events) != 0 || next != nil {
			t.Fatalf("empty ledger query %q: %v / %v, want [] and null", query, events, next)
		}
		if !strings.Contains(recorder.Body.String(), `"events":[]`) {
			t.Fatalf("empty ledger query %q body = %s", query, recorder.Body.String())
		}
	}

	router := NewRouter(openTestStore(t))
	seedComboLedger(t, router)
	day := comboWindow{from: comboDayFrom, to: comboDayTo}

	// Unknown account.
	recorder := comboFetchPage(t, router, comboBuildQuery("nobody", day, 50, 0))
	events, next := decodePage(t, recorder)
	if len(events) != 0 || next != nil {
		t.Fatalf("no-match: %v / %v, want [] and null", events, next)
	}
	// Known account, empty window.
	recorder = comboFetchPage(t, router, comboBuildQuery("alice",
		comboWindow{from: "2026-10-06T00:00:00Z", to: "2026-10-07T00:00:00Z"}, 50, 0))
	events, next = decodePage(t, recorder)
	if len(events) != 0 || next != nil {
		t.Fatalf("window no-match: %v / %v, want [] and null", events, next)
	}
	// Cursor at the last seq, past it, and at the maximum int64.
	for _, cursor := range []int64{255, 256, 9223372036854775807} {
		recorder = comboFetchPage(t, router, comboBuildQuery("alice", day, 50, cursor))
		events, next = decodePage(t, recorder)
		if len(events) != 0 || next != nil {
			t.Fatalf("cursor %d: %v / %v, want [] and null", cursor, events, next)
		}
	}
}

// TestGetEventsCombinedAppendBetweenPages appends one matching record after
// a full first segment has been read, proves later pages see it, and shows a
// re-read of the already-full segment is byte-identical both times.
func TestGetEventsCombinedAppendBetweenPages(t *testing.T) {
	router := NewRouter(openTestStore(t))
	seedComboLedger(t, router)
	mainWindow := comboWindow{from: comboMainFrom, to: comboMainTo}

	firstQuery := comboBuildQuery("alice", mainWindow, 100, 0)
	first := comboFetchPage(t, router, firstQuery)
	firstEvents, firstNext := decodePage(t, first)
	if len(firstEvents) != 100 {
		t.Fatalf("first segment: %d events, want 100", len(firstEvents))
	}
	firstCursor := int64(firstNext.(float64))
	if firstNext != float64(firstEvents[99]["seq"].(float64)) {
		t.Fatalf("first segment cursor = %v, want the last page seq", firstNext)
	}
	firstBody := first.Body.String()

	// Identical re-read before the append returns identical bytes.
	if again := comboFetchPage(t, router, firstQuery); again.Body.String() != firstBody {
		t.Fatalf("full segment changed between identical reads:\n%s\n%s", firstBody, again.Body.String())
	}

	// Append one matching record between page requests. It lands at seq
	// 256 with a half-minute time inside the main window that no seed row
	// uses, so it can be identified by its instant as well as its seq.
	added := mustPost(t, router, eventBody("alice", "2026-10-05T01:30:30Z"))
	if added["seq"] != float64(256) {
		t.Fatalf("interleaved append seq = %v, want 256", added["seq"])
	}

	// The full first segment is still byte-identical after the append.
	if again := comboFetchPage(t, router, firstQuery); again.Body.String() != firstBody {
		t.Fatalf("full segment changed after the interleaved append:\n%s\n%s", firstBody, again.Body.String())
	}

	// Later pages see the new record: expected set is the plan's remainder
	// after the first-page cursor, plus seq 256, in ascending seq order.
	wantTail := comboAliceExpect(mainWindow, firstCursor)
	wantTail = append(wantTail, 256)
	sort.Slice(wantTail, func(i, j int) bool { return wantTail[i] < wantTail[j] })
	recorder := comboFetchPage(t, router, comboBuildQuery("alice", mainWindow, 100, firstCursor))
	tailEvents, tailNext := decodePage(t, recorder)
	comboAssertSeqList(t, "tail with interleaved append", tailEvents, wantTail)
	if tailNext != nil {
		t.Fatalf("tail cursor = %v, want null", tailNext)
	}
	comboAssertRecordFields(t, tailEvents[len(tailEvents)-1], added)

	// Walking the whole window end to end after the append yields all 120
	// original alice rows plus the new one, each once.
	collected, _ := comboWalkPages(t, router, comboPageCase{
		name:     "post-append walk",
		account:  "alice",
		window:   mainWindow,
		limit:    50,
		wantSeqs: wantTail, // placeholder length not used by the walker
	})
	var allWant []int64
	allWant = append(allWant, comboAliceExpect(mainWindow, 0)...)
	allWant = append(allWant, 256)
	got := make([]int64, len(collected))
	for i, item := range collected {
		got[i] = int64(item["seq"].(float64))
	}
	sort.Slice(allWant, func(i, j int) bool { return allWant[i] < allWant[j] })
	if fmt.Sprint(got) != fmt.Sprint(allWant) {
		t.Fatalf("post-append walk seqs = %v, want %v", got, allWant)
	}
}

// TestGetEventsCombinedVerifyAndTailContinuation verifies the chain after
// extensive combined paging: HTTP 200, valid true, checked equal to the
// number of successful appends, then more appends continue from the tail.
func TestGetEventsCombinedVerifyAndTailContinuation(t *testing.T) {
	router := NewRouter(openTestStore(t))
	posted := seedComboLedger(t, router)
	day := comboWindow{from: comboDayFrom, to: comboDayTo}

	// Exercise every page size against the ledger before verifying.
	for _, limit := range []int{1, 50, 100} {
		comboRunPageCase(t, router, posted, comboPageCase{
			name:     fmt.Sprintf("verify prelude limit %d", limit),
			account:  "alice",
			window:   day,
			limit:    limit,
			wantSeqs: comboAliceExpect(day, 0),
		})
	}

	assertComboVerify := func(wantChecked int64) {
		t.Helper()
		recorder := getLedgerVerifyRequest(t, router, "/ledger/verify")
		if recorder.Code != http.StatusOK {
			t.Fatalf("verify: status = %d (%s)", recorder.Code, recorder.Body.String())
		}
		body := decodeVerify(t, recorder)
		if !body.Valid || body.Checked != wantChecked || body.FirstInvalidSeq != nil {
			t.Fatalf("verify = %+v, want valid/%d/null", body, wantChecked)
		}
	}
	assertComboVerify(comboTotalRows)

	// A successful append after verification chains onto the old tail.
	after := mustPost(t, router, eventBody("alice", "2026-10-05T18:00:00Z"))
	if after["seq"] != float64(comboTotalRows+1) {
		t.Fatalf("append after verify: seq = %v, want %d", after["seq"], comboTotalRows+1)
	}
	if after["prev_hash"] != posted[comboTotalRows-1]["hash"] {
		t.Fatalf("append after verify: prev_hash = %v, want seq %d hash %v",
			after["prev_hash"], comboTotalRows, posted[comboTotalRows-1]["hash"])
	}
	assertComboVerify(comboTotalRows + 1)

	// One more append continues from the new tail.
	again := mustPost(t, router, eventBody("bob", "2026-10-05T19:00:00Z"))
	if again["seq"] != float64(comboTotalRows+2) || again["prev_hash"] != after["hash"] {
		t.Fatalf("second append = seq %v prev %v, want %d chained to %v",
			again["seq"], again["prev_hash"], comboTotalRows+2, after["hash"])
	}
	assertComboVerify(comboTotalRows + 2)
}

// assertComboErrorBody checks the published error envelope: exactly one
// top-level error object with string code and message, no partial page and
// no SQL, stack trace or path leakage.
func assertComboErrorBody(t *testing.T, recorder *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status = %d, want %d (%s)", recorder.Code, status, recorder.Body.String())
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &top); err != nil {
		t.Fatalf("decode error body: %v (%s)", err, recorder.Body.String())
	}
	if len(top) != 1 {
		t.Fatalf("top-level keys = %v, want only error", top)
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
	if decoded.Error.Code != code {
		t.Fatalf("code = %q, want %q", decoded.Error.Code, code)
	}
	if decoded.Error.Message == "" {
		t.Fatal("message must be a non-empty string")
	}
	body := recorder.Body.String()
	for _, leak := range []string{"events", "next_after_seq", "SELECT", "INSERT", "sql", "SQL", "goroutine", ".go", "/", "\\"} {
		if strings.Contains(body, leak) {
			t.Fatalf("error body leaks %q or partial page data: %s", leak, body)
		}
	}
}

// TestGetEventsCombinedQueryErrors covers repeated parameters, illegal
// windows and out-of-range pagination values, all with the 400 envelope.
func TestGetEventsCombinedQueryErrors(t *testing.T) {
	router := NewRouter(openTestStore(t))
	seedComboLedger(t, router)
	cases := map[string]string{
		"repeated limit":           "limit=1&limit=2",
		"repeated account same":    "account=alice&account=alice",
		"repeated account differs": "account=alice&account=bob",
		"repeated from":            "from=2026-10-05T00:00:00Z&from=2026-10-05T00:00:00Z",
		"repeated to":              "to=2026-10-05T01:00:00Z&to=2026-10-05T02:00:00Z",
		"repeated after_seq":       "after_seq=1&after_seq=2",
		"unknown parameter":        "account=alice&cursor=1",
		"empty account":            "account=",
		"whitespace account":       "account=%20%09",
		"malformed time":           "from=not-a-time",
		"time without zone":        "to=2026-10-05T00:00:00",
		"leap second":              "from=2016-12-31T23:59:60Z",
		"from after to":            "from=2026-10-05T04:00:00Z&to=2026-10-05T03:00:00Z",
		"from equals to":           "from=2026-10-05T03:00:00Z&to=2026-10-05T03:00:00Z",
		"equal instant fractions":  "from=2026-10-05T03:00:00.5Z&to=2026-10-05T03:00:00.50Z",
		"equal instant offsets":    "from=2026-10-05T11:00:00%2B08:00&to=2026-10-05T03:00:00Z",
		"limit zero":               "limit=0",
		"limit over max":           "limit=101",
		"limit negative":           "limit=-1",
		"limit fraction":           "limit=1.5",
		"limit too large":          "limit=99999999999999999999",
		"after_seq negative":       "after_seq=-1",
		"after_seq overflow":       "after_seq=9223372036854775808",
		"bad percent escape":       "account=%zz",
	}
	for name, query := range cases {
		t.Run(name, func(t *testing.T) {
			assertComboErrorBody(t, getEventsQuery(t, router, query), http.StatusBadRequest, "invalid_audit_input")
		})
	}
}

// TestGetEventsCombinedStorageUnavailableAndPrecedence closes the store and
// proves parameter errors still return 400 while legal requests return 503
// storage_unavailable, both with the bare error envelope.
func TestGetEventsCombinedStorageUnavailableAndPrecedence(t *testing.T) {
	st := openTestStore(t)
	router := NewRouter(st)
	mustPost(t, router, eventBody("alice", "2026-10-05T00:00:00Z"))
	st.Close()

	for _, query := range []string{
		"limit=0",
		"limit=1&limit=2",
		"from=2026-10-05T04:00:00Z&to=2026-10-05T03:00:00Z",
		"from=2026-10-05T03:00:00Z&to=2026-10-05T03:00:00Z",
		"after_seq=-1",
		"account=",
	} {
		assertComboErrorBody(t, getEventsQuery(t, router, query), http.StatusBadRequest, "invalid_audit_input")
	}

	for _, query := range []string{
		"",
		"account=alice",
		"account=alice&from=2026-10-05T00:00:00Z&to=2026-10-06T00:00:00Z&limit=1&after_seq=0",
		"limit=100",
	} {
		assertComboErrorBody(t, getEventsQuery(t, router, query), http.StatusServiceUnavailable, "storage_unavailable")
	}
}

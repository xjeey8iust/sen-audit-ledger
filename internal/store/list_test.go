package store

import (
	"path/filepath"
	"testing"
)

func TestCompareInstants(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want int
	}{
		{"identical", "2026-10-05T00:00:00Z", "2026-10-05T00:00:00Z", 0},
		{"fraction zero equals none", "2026-10-05T00:00:00Z", "2026-10-05T00:00:00.0Z", 0},
		{"trailing zeros carry no weight", "2026-10-05T00:00:00.5Z", "2026-10-05T00:00:00.50Z", 0},
		{"shorter fraction smaller", "2026-10-05T00:00:00.25Z", "2026-10-05T00:00:00.5Z", -1},
		{"long fractions beyond nanoseconds", "2026-10-05T00:00:00.0000000001Z", "2026-10-05T00:00:00.00000000009Z", 1},
		{"tiny fraction above zero", "2026-10-05T00:00:00Z", "2026-10-05T00:00:00.000000000000001Z", -1},
		{"second beats fraction", "2026-10-05T00:00:01Z", "2026-10-05T00:00:00.999999Z", 1},
		{"head ordering", "2026-10-05T00:00:00Z", "2026-10-06T00:00:00Z", -1},
		{"year range extremes", "0000-01-01T00:00:00Z", "9999-12-31T23:59:59.999Z", -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CompareInstants(tc.a, tc.b); got != tc.want {
				t.Fatalf("CompareInstants(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
			if got := CompareInstants(tc.b, tc.a); got != -tc.want {
				t.Fatalf("CompareInstants(%q, %q) = %d, want %d", tc.b, tc.a, got, -tc.want)
			}
		})
	}
}

// seedListStore appends one record per (account, occurredAt) pair and returns
// the open store.
func seedListStore(t *testing.T, accounts, occurredAts []string) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	for i := range accounts {
		_, err := st.Append(EventInput{
			Account:    accounts[i],
			Operation:  "op",
			Resource:   "res",
			Result:     "ok",
			OccurredAt: occurredAts[i],
		})
		if err != nil {
			t.Fatalf("append %d: %v", i+1, err)
		}
	}
	return st
}

func wantListSeqs(t *testing.T, events []Event, want ...int64) {
	t.Helper()
	if len(events) != len(want) {
		t.Fatalf("seqs = %v, want %v", events, want)
	}
	for i, event := range events {
		if event.Seq != want[i] {
			t.Fatalf("seqs[%d] = %d, want %v", i, event.Seq, want)
		}
	}
}

func TestListEventsEmptyLedger(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	events, hasMore, err := st.ListEvents(EventFilter{Limit: 50})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(events) != 0 || hasMore {
		t.Fatalf("empty ledger = %v events, hasMore %v; want 0 and false", len(events), hasMore)
	}
}

func TestListEventsFilters(t *testing.T) {
	st := seedListStore(t,
		[]string{"alice", "bob", "alice", "alice"},
		[]string{
			"2026-10-05T00:00:00Z",    // seq 1
			"2026-10-05T00:00:00.5Z",  // seq 2
			"2026-10-05T01:00:00Z",    // seq 3
			"2026-10-05T02:00:00.25Z", // seq 4
		},
	)

	// Account matching is exact: case and whitespace are significant.
	events, hasMore, err := st.ListEvents(EventFilter{Account: ptr("alice"), Limit: 50})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	wantListSeqs(t, events, 1, 3, 4)
	if hasMore {
		t.Fatal("hasMore = true, want false")
	}

	events, _, err = st.ListEvents(EventFilter{Account: ptr("Alice"), Limit: 50})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	wantListSeqs(t, events)

	// The window includes from and excludes to, comparing instants.
	from, to := "2026-10-05T00:00:00.50Z", "2026-10-05T02:00:00.25Z"
	events, _, err = st.ListEvents(EventFilter{From: from, HasFrom: true, To: to, HasTo: true, Limit: 50})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	wantListSeqs(t, events, 2, 3)

	// after_seq pages upward strictly past the cursor.
	events, hasMore, err = st.ListEvents(EventFilter{AfterSeq: 2, Limit: 1})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	wantListSeqs(t, events, 3)
	if !hasMore {
		t.Fatal("hasMore = false, want true")
	}

	// A cursor past the tail matches nothing.
	events, hasMore, err = st.ListEvents(EventFilter{AfterSeq: 4, Limit: 50})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	wantListSeqs(t, events)
	if hasMore {
		t.Fatal("hasMore = true, want false")
	}
}

func TestListEventsLimitAndHasMore(t *testing.T) {
	st := seedListStore(t,
		[]string{"a", "a", "a", "a", "a"},
		[]string{
			"2026-10-01T00:00:00Z", "2026-10-02T00:00:00Z", "2026-10-03T00:00:00Z",
			"2026-10-04T00:00:00Z", "2026-10-05T00:00:00Z",
		},
	)

	events, hasMore, err := st.ListEvents(EventFilter{Limit: 2})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	wantListSeqs(t, events, 1, 2)
	if !hasMore {
		t.Fatal("hasMore = false, want true")
	}

	// Exactly one full page: no further records, so hasMore is false.
	events, hasMore, err = st.ListEvents(EventFilter{AfterSeq: 3, Limit: 2})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	wantListSeqs(t, events, 4, 5)
	if hasMore {
		t.Fatal("hasMore = true, want false")
	}
}

func ptr(s string) *string { return &s }

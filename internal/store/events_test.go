package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

func openTempStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func input(account string) EventInput {
	return EventInput{
		Account:    account,
		Operation:  "login",
		Resource:   "console",
		Result:     "success",
		OccurredAt: "2026-10-05T00:30:00.250Z",
	}
}

// expectedHash recomputes the documented digest independently of the
// implementation under test for inputs that need no JSON escaping.
func expectedHash(seq uint64, account, operation, resource, result, occurredAt, prevHash string) string {
	payload := fmt.Sprintf(`[%d,"%s","%s","%s","%s","%s","%s"]`,
		seq, account, operation, resource, result, occurredAt, prevHash)
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

func TestAppendAssignsSequentialSeqAndChain(t *testing.T) {
	st := openTempStore(t)

	first, err := st.Append(input("alice"))
	if err != nil {
		t.Fatalf("append first: %v", err)
	}
	if first.Seq != 1 {
		t.Fatalf("first seq = %d, want 1", first.Seq)
	}
	if first.PrevHash != zeroPrevHash {
		t.Fatalf("first prev_hash = %q, want 64 zeros", first.PrevHash)
	}
	want := expectedHash(1, "alice", "login", "console", "success", "2026-10-05T00:30:00.250Z", zeroPrevHash)
	if first.Hash != want {
		t.Fatalf("first hash = %q, want %q", first.Hash, want)
	}

	second, err := st.Append(input("bob"))
	if err != nil {
		t.Fatalf("append second: %v", err)
	}
	if second.Seq != 2 {
		t.Fatalf("second seq = %d, want 2", second.Seq)
	}
	if second.PrevHash != first.Hash {
		t.Fatalf("second prev_hash = %q, want first hash %q", second.PrevHash, first.Hash)
	}
	want = expectedHash(2, "bob", "login", "console", "success", "2026-10-05T00:30:00.250Z", first.Hash)
	if second.Hash != want {
		t.Fatalf("second hash = %q, want %q", second.Hash, want)
	}
}

func TestAppendExplicitSeq(t *testing.T) {
	st := openTempStore(t)

	one := uint64(1)
	if _, err := st.Append(EventInput{Account: "a", Operation: "o", Resource: "r", Result: "ok", OccurredAt: "2026-10-05T00:00:00Z", Seq: &one}); err != nil {
		t.Fatalf("append explicit seq 1: %v", err)
	}
	if _, err := st.Append(EventInput{Account: "a", Operation: "o", Resource: "r", Result: "ok", OccurredAt: "2026-10-05T00:00:00Z", Seq: &one}); !errors.Is(err, ErrSeqConflict) {
		t.Fatalf("repeated seq 1 error = %v, want ErrSeqConflict", err)
	}
	three := uint64(3)
	if _, err := st.Append(EventInput{Account: "a", Operation: "o", Resource: "r", Result: "ok", OccurredAt: "2026-10-05T00:00:00Z", Seq: &three}); !errors.Is(err, ErrSeqNotNext) {
		t.Fatalf("gap seq 3 error = %v, want ErrSeqNotNext", err)
	}
	two := uint64(2)
	event, err := st.Append(EventInput{Account: "a", Operation: "o", Resource: "r", Result: "ok", OccurredAt: "2026-10-05T00:00:00Z", Seq: &two})
	if err != nil {
		t.Fatalf("append explicit seq 2: %v", err)
	}
	if event.Seq != 2 {
		t.Fatalf("seq = %d, want 2", event.Seq)
	}
	// A failed append must not consume a sequence number or move the tail.
	next, err := st.Append(input("carol"))
	if err != nil {
		t.Fatalf("append after failures: %v", err)
	}
	if next.Seq != 3 {
		t.Fatalf("seq after failures = %d, want 3", next.Seq)
	}
	if next.PrevHash != event.Hash {
		t.Fatalf("prev_hash after failures = %q, want %q", next.PrevHash, event.Hash)
	}
}

func TestReopenContinuesSequenceAndChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")

	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	first, err := st.Append(input("alice"))
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	second, err := reopened.Append(input("bob"))
	if err != nil {
		t.Fatalf("append after reopen: %v", err)
	}
	if second.Seq != 2 {
		t.Fatalf("seq after reopen = %d, want 2", second.Seq)
	}
	if second.PrevHash != first.Hash {
		t.Fatalf("prev_hash after reopen = %q, want %q", second.PrevHash, first.Hash)
	}
}

func TestConcurrentAppendsStayGapFree(t *testing.T) {
	st := openTempStore(t)

	const writers = 16
	results := make([]Event, writers)
	errs := make([]error, writers)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = st.Append(input(fmt.Sprintf("user-%02d", i)))
		}(i)
	}
	wg.Wait()

	seen := make(map[uint64]Event, writers)
	for i := 0; i < writers; i++ {
		if errs[i] != nil {
			t.Fatalf("append %d: %v", i, errs[i])
		}
		if _, dup := seen[results[i].Seq]; dup {
			t.Fatalf("seq %d assigned twice", results[i].Seq)
		}
		seen[results[i].Seq] = results[i]
	}
	prevHash := zeroPrevHash
	for seq := uint64(1); seq <= writers; seq++ {
		event, ok := seen[seq]
		if !ok {
			t.Fatalf("seq %d missing: hole in the chain", seq)
		}
		if event.PrevHash != prevHash {
			t.Fatalf("seq %d prev_hash = %q, want %q", seq, event.PrevHash, prevHash)
		}
		want := expectedHash(seq, event.Account, event.Operation, event.Resource, event.Result, event.OccurredAt, prevHash)
		if event.Hash != want {
			t.Fatalf("seq %d hash = %q, want %q", seq, event.Hash, want)
		}
		prevHash = event.Hash
	}
}

func TestChainHashEscapesOnlyQuoteBackslashAndControls(t *testing.T) {
	account := "a\"b\\c\n<>&é"
	payload := `[1,"a\"b\\c` + "\\u000a" + `<>&é","op","res","ok","2026-10-05T00:00:00Z","` + zeroPrevHash + `"]`
	sum := sha256.Sum256([]byte(payload))
	if got := chainHash(1, account, "op", "res", "ok", "2026-10-05T00:00:00Z", zeroPrevHash); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("chainHash = %q, want digest of %q", got, payload)
	}
}

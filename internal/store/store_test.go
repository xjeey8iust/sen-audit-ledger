package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"
)

func TestOpenCreatesUsableStore(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	if err := st.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

func TestComputeHashEscapesOnlyQuoteBackslashAndControls(t *testing.T) {
	event := Event{
		Seq:        7,
		Account:    "a\"b\\c",
		Operation:  "line\nbreak\ttab",
		Resource:   "res<&>é",
		Result:     "ok",
		OccurredAt: "2026-10-05T00:00:00.5Z",
		PrevHash:   "abc123",
	}
	// Control characters appear as lowercase xx escapes (not \n or \t);
	// non-ASCII and characters like < & > stay raw.
	payload := "[7,\"a\\\"b\\\\c\",\"line\\u000abreak\\u0009tab\",\"res<&>é\",\"ok\",\"2026-10-05T00:00:00.5Z\",\"abc123\"]"
	sum := sha256.Sum256([]byte(payload))
	want := hex.EncodeToString(sum[:])
	if got := computeHash(event); got != want {
		t.Fatalf("computeHash = %s, want %s", got, want)
	}
}

func TestAppendAssignsSequenceAndChains(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	input := EventInput{Account: "a", Operation: "o", Resource: "r", Result: "ok", OccurredAt: "2026-10-05T00:00:00Z"}
	first, err := st.Append(input)
	if err != nil {
		t.Fatalf("append first: %v", err)
	}
	if first.Seq != 1 || first.PrevHash != genesisPrevHash {
		t.Fatalf("first = %+v", first)
	}
	second, err := st.Append(input)
	if err != nil {
		t.Fatalf("append second: %v", err)
	}
	if second.Seq != 2 || second.PrevHash != first.Hash {
		t.Fatalf("second = %+v, want seq 2 chained to %s", second, first.Hash)
	}

	explicit := int64(2)
	if _, err := st.Append(EventInput{Seq: &explicit, Account: "a", Operation: "o", Resource: "r", Result: "ok", OccurredAt: "2026-10-05T00:00:00Z"}); !errors.Is(err, ErrSeqConflict) {
		t.Fatalf("existing seq: err = %v, want ErrSeqConflict", err)
	}
	explicit = 9
	if _, err := st.Append(EventInput{Seq: &explicit, Account: "a", Operation: "o", Resource: "r", Result: "ok", OccurredAt: "2026-10-05T00:00:00Z"}); !errors.Is(err, ErrSeqGap) {
		t.Fatalf("gap seq: err = %v, want ErrSeqGap", err)
	}
	// Failed appends leave the tail untouched.
	third, err := st.Append(input)
	if err != nil {
		t.Fatalf("append third: %v", err)
	}
	if third.Seq != 3 || third.PrevHash != second.Hash {
		t.Fatalf("third = %+v, want seq 3 chained to %s", third, second.Hash)
	}
}

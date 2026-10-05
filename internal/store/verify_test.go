package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// verifyInput builds one append input shared by the verification tests.
func verifyInput(account string) EventInput {
	return EventInput{
		Account:    account,
		Operation:  "login",
		Resource:   "console",
		Result:     "ok",
		OccurredAt: "2026-10-05T00:30:00.250Z",
	}
}

// seedVerifiableChain appends n valid records and returns the resulting store.
func seedVerifiableChain(t *testing.T, path string, n int) *Store {
	t.Helper()
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for i := 0; i < n; i++ {
		if _, err := st.Append(verifyInput("alice")); err != nil {
			t.Fatalf("append %d: %v", i+1, err)
		}
	}
	return st
}

// rawDB opens a connection to the database file independent of the service so
// tests can corrupt rows the append API itself would never produce.
func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestVerifyChainEmptyLedger(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	result, err := st.VerifyChain()
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !result.Valid || result.Checked != 0 || result.FirstInvalidSeq != nil {
		t.Fatalf("empty ledger = %+v, want valid/0/nil", result)
	}
}

func TestVerifyChainAcceptsCompleteChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	st := seedVerifiableChain(t, path, 5)
	defer st.Close()

	result, err := st.VerifyChain()
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !result.Valid || result.Checked != 5 || result.FirstInvalidSeq != nil {
		t.Fatalf("complete chain = %+v, want valid/5/nil", result)
	}
}

func TestVerifyChainAcceptsLegalPrefix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	st := seedVerifiableChain(t, path, 4)

	// Removing the tail records leaves a legal chain prefix starting at 1.
	if _, err := rawDB(t, path).Exec("DELETE FROM events WHERE seq >= 3"); err != nil {
		t.Fatalf("trim chain: %v", err)
	}

	result, err := st.VerifyChain()
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !result.Valid || result.Checked != 2 || result.FirstInvalidSeq != nil {
		t.Fatalf("legal prefix = %+v, want valid/2/nil", result)
	}
}

func TestVerifyChainGapReturnsExpectedSeq(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	st := seedVerifiableChain(t, path, 4)
	defer st.Close()

	// Drop seq 2: the remaining rows are 1 and 3.
	if _, err := rawDB(t, path).Exec("DELETE FROM events WHERE seq = 2"); err != nil {
		t.Fatalf("open gap: %v", err)
	}

	result, err := st.VerifyChain()
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	// Seq 1 passes; the next stored row is seq 3 while the expected seq is 2.
	// checked includes the row that triggered the failure, so it is 2.
	if result.Valid || result.Checked != 2 || result.FirstInvalidSeq == nil || *result.FirstInvalidSeq != 2 {
		t.Fatalf("gap = %+v, want invalid/2/first 2", result)
	}
}

func TestVerifyChainGenesisGapStopsAtFirstRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	st := seedVerifiableChain(t, path, 3)
	defer st.Close()

	// Drop seq 1: the first stored row is seq 2, expected seq is 1.
	if _, err := rawDB(t, path).Exec("DELETE FROM events WHERE seq = 1"); err != nil {
		t.Fatalf("remove genesis: %v", err)
	}

	result, err := st.VerifyChain()
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if result.Valid || result.Checked != 1 || result.FirstInvalidSeq == nil || *result.FirstInvalidSeq != 1 {
		t.Fatalf("missing genesis = %+v, want invalid/1/first 1", result)
	}
}

func TestVerifyChainBrokenPrevHashReturnsRecordSeq(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	st := seedVerifiableChain(t, path, 4)
	defer st.Close()

	if _, err := rawDB(t, path).Exec("UPDATE events SET prev_hash = ? WHERE seq = 3", genesisPrevHash); err != nil {
		t.Fatalf("break prev_hash: %v", err)
	}

	result, err := st.VerifyChain()
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	// Seqs 1 and 2 pass; record 3 has a wrong back-link (its stored hash was
	// computed over the correct prev_hash, so its own hash fails too).
	if result.Valid || result.Checked != 3 || result.FirstInvalidSeq == nil || *result.FirstInvalidSeq != 3 {
		t.Fatalf("broken prev_hash = %+v, want invalid/3/first 3", result)
	}
}

func TestVerifyChainTamperedHashReturnsRecordSeq(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	st := seedVerifiableChain(t, path, 3)
	defer st.Close()

	if _, err := rawDB(t, path).Exec("UPDATE events SET hash = ? WHERE seq = 2", genesisPrevHash); err != nil {
		t.Fatalf("break hash: %v", err)
	}

	result, err := st.VerifyChain()
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if result.Valid || result.Checked != 2 || result.FirstInvalidSeq == nil || *result.FirstInvalidSeq != 2 {
		t.Fatalf("tampered hash = %+v, want invalid/2/first 2", result)
	}

	// Verification never repairs: the tampered value is still stored.
	var stored string
	if err := rawDB(t, path).QueryRow("SELECT hash FROM events WHERE seq = 2").Scan(&stored); err != nil {
		t.Fatalf("read tampered row: %v", err)
	}
	if stored != genesisPrevHash {
		t.Fatalf("verify rewrote the stored hash: got %s", stored)
	}
}

func TestVerifyChainTamperedFieldReturnsRecordSeq(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	st := seedVerifiableChain(t, path, 3)
	defer st.Close()

	if _, err := rawDB(t, path).Exec("UPDATE events SET account = ? WHERE seq = 2", "mallory"); err != nil {
		t.Fatalf("tamper field: %v", err)
	}

	result, err := st.VerifyChain()
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if result.Valid || result.Checked != 2 || result.FirstInvalidSeq == nil || *result.FirstInvalidSeq != 2 {
		t.Fatalf("tampered field = %+v, want invalid/2/first 2", result)
	}
}

func TestVerifyChainStopsAtFirstInvalidRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	st := seedVerifiableChain(t, path, 5)
	defer st.Close()
	db := rawDB(t, path)

	// Both seq 2 and seq 4 are broken; the pass must stop on seq 2.
	if _, err := db.Exec("UPDATE events SET hash = ? WHERE seq = 2", genesisPrevHash); err != nil {
		t.Fatalf("break seq 2: %v", err)
	}
	if _, err := db.Exec("UPDATE events SET hash = ? WHERE seq = 4", genesisPrevHash); err != nil {
		t.Fatalf("break seq 4: %v", err)
	}

	result, err := st.VerifyChain()
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if result.Valid || result.Checked != 2 || result.FirstInvalidSeq == nil || *result.FirstInvalidSeq != 2 {
		t.Fatalf("multiple breaks = %+v, want stop at invalid/2/first 2", result)
	}
}

func TestVerifyChainDoesNotMutateRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	st := seedVerifiableChain(t, path, 3)
	defer st.Close()
	db := rawDB(t, path)

	if _, err := db.Exec("UPDATE events SET account = ?, hash = ? WHERE seq = 2", "mallory", genesisPrevHash); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	if _, err := st.VerifyChain(); err != nil {
		t.Fatalf("verify: %v", err)
	}
	// Run a second time: the tampered rows are still exactly as left, with no
	// backfilled hashes or repaired fields.
	var account, hash string
	if err := db.QueryRow("SELECT account, hash FROM events WHERE seq = 2").Scan(&account, &hash); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if account != "mallory" || hash != genesisPrevHash {
		t.Fatalf("row changed by verify: account = %q hash = %s", account, hash)
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM events").Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 3 {
		t.Fatalf("row count = %d, verify must not fill gaps or delete rows", count)
	}
}

func TestVerifyChainSpecialCharactersStillVerify(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	records := []EventInput{
		{Account: "管理员\"甲", Operation: "op", Resource: "r", Result: "ok", OccurredAt: "2026-10-05T00:00:00Z"},
		{Account: `a\b`, Operation: "line\nbreak", Resource: "r\t?", Result: "ok", OccurredAt: "2026-10-05T00:00:00.5Z"},
	}
	for _, in := range records {
		if _, err := st.Append(in); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	result, err := st.VerifyChain()
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !result.Valid || result.Checked != 2 || result.FirstInvalidSeq != nil {
		t.Fatalf("special-character chain = %+v, want valid/2/nil", result)
	}
}

func TestVerifyChainStorageFailure(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if _, err := st.VerifyChain(); err == nil {
		t.Fatal("verify on closed store: want error, got nil")
	}
}

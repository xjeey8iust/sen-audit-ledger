package api

// Regression tests for the public append entry with special characters:
//
//   - every control character U+0000..U+001F is placed inside visible text
//     in each of the four business fields (account, operation, resource,
//     result) and followed through decode, hash and persistence, checking
//     the 201 response and the SQLite rows keep the text verbatim;
//   - mixed text with a double quote, a backslash, Chinese, a supplementary
//     plane character, <, >, &, U+2028 and U+2029 round-trips unchanged;
//   - equivalent requests (short escapes, \uXXXX escapes either case, direct
//     characters, shuffled keys, whitespace around the object, equivalent
//     timezone offsets) at the same chain position produce the same hash;
//   - raw control bytes inside strings and business text that decodes to
//     only whitespace are rejected with 400 invalid_audit_input, leaving
//     the stored records, the chain tail and the next sequence number
//     untouched;
//   - two special-character records survive a close/reopen of the same
//     database with text and hashes intact, and the next append chains
//     onto the original tail.
//
// Every expected hash is derived independently from the public rule in
// README.md (compact JSON array, '"' and '\' escaped, control characters
// as lowercase \u00xx, everything else raw UTF-8) and anchored to
// hardcoded test vectors; neither the service's returned hash nor the
// product's internal hash function is used as an expectation.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// hashByPublicRule recomputes a record hash from the documented public
// rule: SHA-256 over the compact JSON array
// [seq, account, operation, resource, result, occurred_at, prev_hash]
// encoded as UTF-8.
func hashByPublicRule(seq int64, account, operation, resource, result, occurredAt, prevHash string) string {
	payload := "[" + fmt.Sprintf("%d", seq)
	for _, field := range []string{account, operation, resource, result, occurredAt, prevHash} {
		payload += ",\"" + escapeByPublicRule(field) + "\""
	}
	payload += "]"
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

// escapeByPublicRule implements the string escaping of the public hash
// rule: the double quote and the backslash are backslash-escaped, control
// characters below U+0020 become a six-character \u00xx escape with
// lowercase hex, and every other character keeps its raw UTF-8 bytes.
func escapeByPublicRule(s string) string {
	const hexdigits = "0123456789abcdef"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
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
	return b.String()
}

// mixedResult is the canonical decoded result field used by the mixed-text
// tests: visible text followed by a double quote, a backslash, U+2028 and
// U+2029.
const mixedResult = "ok\"\\\u2028\u2029"

// TestPublicRuleHashVectors pins the test-side hash implementation to
// fixed vectors computed independently from the README rule, so a shared
// mistake in hashByPublicRule cannot go unnoticed.
func TestPublicRuleHashVectors(t *testing.T) {
	genesis := strings.Repeat("0", 64)
	vectors := []struct {
		name                                 string
		seq                                  int64
		account, operation, resource, result string
		occurredAt, prevHash, wantHash       string
	}{
		{
			name: "control character NUL in account",
			seq:  1,
			// Payload: [1,"a\u0000b","o","r","ok","2026-10-05T00:30:00Z","00…00"]
			account: "a\x00b", operation: "o", resource: "r", result: "ok",
			occurredAt: "2026-10-05T00:30:00Z", prevHash: genesis,
			wantHash: "2bb703ec6e1e2e445f190be444c4912af712b57a6a1053cb760caecfa3210789",
		},
		{
			name: "mixed quote backslash CJK supplementary and separators",
			seq:  1,
			// Payload: [1,"ali\u0009ce中","re\u000aad","r😀<>&","ok\"\\<U+2028><U+2029>","2026-10-05T00:30:00.5Z","00…00"]
			account: "ali\tce中", operation: "re\nad", resource: "r😀<>&",
			result:     mixedResult,
			occurredAt: "2026-10-05T00:30:00.5Z", prevHash: genesis,
			wantHash: "0e3aa013f2b09a6ae6ed61b6d0196579fc3c3a8d1f8dc5cb19f25d58b5c9af77",
		},
	}
	for _, v := range vectors {
		t.Run(v.name, func(t *testing.T) {
			got := hashByPublicRule(v.seq, v.account, v.operation, v.resource, v.result, v.occurredAt, v.prevHash)
			if got != v.wantHash {
				t.Fatalf("hashByPublicRule = %s, want %s", got, v.wantHash)
			}
		})
	}
}

// TestPostEventControlCharactersFullPath posts every control character
// U+0000..U+001F inside visible text in each business field and verifies
// the whole path: 201 response, decoded text kept verbatim, UTC
// normalization, independently derived hash, and the persisted SQLite rows.
func TestPostEventControlCharactersFullPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st := openStoreAt(t, path)
	router := NewRouter(st)

	const occurredUTC = "2026-10-05T00:30:00Z"
	fieldNames := []string{"account", "operation", "resource", "result"}

	type wantRow struct {
		account, operation, resource, result string
	}
	var wants []wantRow
	var wantHashes []string

	prev := strings.Repeat("0", 64)
	seq := int64(0)
	for cp := 0; cp < 0x20; cp++ {
		cp := cp
		t.Run(fmt.Sprintf("U+%04X", cp), func(t *testing.T) {
			for _, field := range fieldNames {
				seq++
				decoded := map[string]string{
					"account": "acct", "operation": "op", "resource": "res", "result": "ok",
				}
				decoded[field] = "pre" + string(rune(cp)) + "post"

				// The control character travels as a \u00xx escape; the
				// visible characters around it must survive untouched.
				esc := fmt.Sprintf(`\u%04x`, cp)
				jsonValue := func(name string) string {
					if name == field {
						return `"pre` + esc + `post"`
					}
					return `"` + decoded[name] + `"`
				}
				body := `{"account":` + jsonValue("account") +
					`,"operation":` + jsonValue("operation") +
					`,"resource":` + jsonValue("resource") +
					`,"result":` + jsonValue("result") +
					`,"occurred_at":"2026-10-05T08:30:00+08:00"}`

				recorder := postEventBody(t, router, body)
				if recorder.Code != http.StatusCreated {
					t.Fatalf("%s: status = %d, want 201 (%s)", field, recorder.Code, recorder.Body.String())
				}
				got := decodeCreated(t, recorder)
				if got.Account != decoded["account"] || got.Operation != decoded["operation"] ||
					got.Resource != decoded["resource"] || got.Result != decoded["result"] {
					t.Fatalf("%s: response fields = %+v, want %+v", field, got, decoded)
				}
				if got.OccurredAt != occurredUTC {
					t.Fatalf("%s: occurred_at = %q, want %q", field, got.OccurredAt, occurredUTC)
				}
				if got.Seq != seq || got.PrevHash != prev {
					t.Fatalf("%s: seq = %d prev_hash = %s, want %d and %s", field, got.Seq, got.PrevHash, seq, prev)
				}
				wantHash := hashByPublicRule(seq, decoded["account"], decoded["operation"],
					decoded["resource"], decoded["result"], occurredUTC, prev)
				if got.Hash != wantHash {
					t.Fatalf("%s: hash = %s, want %s per the public rule", field, got.Hash, wantHash)
				}
				wants = append(wants, wantRow{decoded["account"], decoded["operation"], decoded["resource"], decoded["result"]})
				wantHashes = append(wantHashes, wantHash)
				prev = wantHash
			}
		})
	}

	// Close, then read the file through an independent connection: every
	// row keeps the decoded text verbatim, the chain is contiguous and
	// each stored hash matches the independently derived expectation.
	closeStore(t, st)
	events := readFullLedger(t, path)
	if len(events) != len(wants) {
		t.Fatalf("persisted %d events, want %d", len(events), len(wants))
	}
	prev = strings.Repeat("0", 64)
	for i, want := range wants {
		e := events[i]
		if e.seq != int64(i+1) {
			t.Fatalf("row %d: seq = %d, want %d", i, e.seq, i+1)
		}
		if e.account != want.account || e.operation != want.operation ||
			e.resource != want.resource || e.result != want.result {
			t.Fatalf("row %d: stored fields = %+v, want %+v", i, e, want)
		}
		if e.occurredAt != occurredUTC {
			t.Fatalf("row %d: occurred_at = %q, want %q", i, e.occurredAt, occurredUTC)
		}
		if e.prevHash != prev {
			t.Fatalf("row %d: prev_hash = %s, want %s", i, e.prevHash, prev)
		}
		if e.hash != wantHashes[i] {
			t.Fatalf("row %d: hash = %s, want %s per the public rule", i, e.hash, wantHashes[i])
		}
		prev = wantHashes[i]
	}
}

// TestPostEventMixedTextEquivalentEncodingsShareHash submits one logical
// event — containing a tab, a newline, a double quote, a backslash,
// Chinese, a supplementary plane character, <, >, &, U+2028 and U+2029 —
// in differently encoded but equivalent requests. At the same chain
// position (same seq, same prev_hash) every variant must produce the hash
// of the fixed test vector, and the persisted text must be identical.
func TestPostEventMixedTextEquivalentEncodingsShareHash(t *testing.T) {
	// The canonical decoded text of the four business fields.
	account := "ali\tce中"
	operation := "re\nad"
	resource := "r😀<>&"
	result := mixedResult
	const occurredUTC = "2026-10-05T00:30:00.5Z"
	// Fixed vector for this exact event at seq 1 chained to genesis.
	const wantHash = "0e3aa013f2b09a6ae6ed61b6d0196579fc3c3a8d1f8dc5cb19f25d58b5c9af77"
	genesis := strings.Repeat("0", 64)

	variants := map[string]string{
		// Short escapes for the control characters, everything else direct
		// (including raw U+2028/U+2029, which JSON strings may carry).
		"short escapes": `{"account":"ali\tce中","operation":"re\nad","resource":"r😀<>&",` +
			`"result":"ok\"\\` + "\u2028\u2029" + `","occurred_at":"2026-10-05T08:30:00.5+08:00"}`,
		// Every non-ASCII or special character as a lowercase \uXXXX escape.
		"unicode escapes lowercase": `{"account":"ali\u0009ce\u4e2d","operation":"re\u000aad",` +
			`"resource":"r\ud83d\ude00\u003c\u003e\u0026","result":"ok\"\\\u2028\u2029",` +
			`"occurred_at":"2026-10-05T08:30:00.5+08:00"}`,
		// Uppercase hex escapes, shuffled keys, whitespace around the
		// object and between tokens.
		"uppercase shuffled whitespace": "  {\n" +
			`  "result": "ok\"\\\u2028\u2029",` + "\n" +
			`  "occurred_at": "2026-10-05T08:30:00.5+08:00",` + "\n" +
			`  "resource": "r\uD83D\uDE00\u003C\u003E\u0026",` + "\n" +
			`  "operation": "re\u000Aad",` + "\n" +
			`  "account": "ali\u0009ce\u4E2D"` + "\n" +
			"} \t\r\n",
		// Direct characters wherever JSON allows them, \uXXXX for the
		// control characters, shuffled keys, and a different timezone
		// offset that normalizes to the same UTC instant.
		"direct chars equivalent offset": `{"occurred_at":"2026-10-05T02:30:00.5+02:00",` +
			`"result":"ok\"\\` + "\u2028\u2029" + `","resource":"r😀<>&",` +
			`"operation":"re\u000aad","account":"ali\u0009ce中"}`,
	}

	for name, body := range variants {
		t.Run(name, func(t *testing.T) {
			// Each variant appends at the same chain position: a fresh
			// ledger where seq 1 chains to genesis.
			path := filepath.Join(t.TempDir(), "service.db")
			st := openStoreAt(t, path)
			recorder := postEventBody(t, NewRouter(st), body)
			if recorder.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201 (%s)", recorder.Code, recorder.Body.String())
			}
			got := decodeCreated(t, recorder)
			if got.Account != account || got.Operation != operation ||
				got.Resource != resource || got.Result != result {
				t.Fatalf("decoded fields = %+v, want the canonical text", got)
			}
			if got.OccurredAt != occurredUTC {
				t.Fatalf("occurred_at = %q, want %q", got.OccurredAt, occurredUTC)
			}
			if got.Seq != 1 || got.PrevHash != genesis {
				t.Fatalf("seq = %d prev_hash = %s, want 1 and genesis", got.Seq, got.PrevHash)
			}
			if got.Hash != wantHash {
				t.Fatalf("hash = %s, want the fixed vector %s", got.Hash, wantHash)
			}
			closeStore(t, st)

			// The persisted row carries the same decoded text and hash.
			events := readFullLedger(t, path)
			if len(events) != 1 {
				t.Fatalf("persisted %d events, want 1", len(events))
			}
			e := events[0]
			if e.account != account || e.operation != operation ||
				e.resource != resource || e.result != result || e.occurredAt != occurredUTC {
				t.Fatalf("stored fields = %+v, want the canonical text", e)
			}
			if e.seq != 1 || e.prevHash != genesis || e.hash != wantHash {
				t.Fatalf("stored chain = seq %d prev %s hash %s, want 1/genesis/%s",
					e.seq, e.prevHash, e.hash, wantHash)
			}
		})
	}
}

// TestPostEventRejectsRawControlBytesAndBlankText covers the two illegal
// branches around special text: unescaped control bytes inside strings,
// and business text that decodes to whitespace only. Both return 400
// invalid_audit_input and leave the ledger, the tail hash and the next
// sequence number untouched.
func TestPostEventRejectsRawControlBytesAndBlankText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st := openStoreAt(t, path)
	defer closeStore(t, st)
	router := NewRouter(st)

	// Seed one valid record so the chain has a tail to protect.
	genesis := strings.Repeat("0", 64)
	seedBody := `{"account":"alice","operation":"login","resource":"console","result":"ok","occurred_at":"2026-10-05T08:30:00+08:00"}`
	recorder := postEventBody(t, router, seedBody)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("seed: status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	seed := decodeCreated(t, recorder)
	seedHash := hashByPublicRule(1, "alice", "login", "console", "ok", "2026-10-05T00:30:00Z", genesis)
	if seed.Hash != seedHash {
		t.Fatalf("seed hash = %s, want %s per the public rule", seed.Hash, seedHash)
	}

	// Raw control bytes U+0000..U+001F inside a string are not valid JSON
	// and must be rejected, one request per byte.
	for cp := 0; cp < 0x20; cp++ {
		body := `{"account":"a` + string(byte(cp)) +
			`b","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z"}`
		wantInvalidInput(t, router, body)
	}

	// Business text that decodes to whitespace only is rejected in every
	// one of the four fields.
	blanks := []struct {
		name  string
		value string // JSON string literal
	}{
		{"spaces and tab", `" \u0009 "`},
		{"control whitespace only", `"\u000a\u000b\u000c\u000d"`},
		{"non-breaking space", `"\u00a0"`},
		{"ideographic space", `"\u3000"`},
	}
	for _, field := range []string{"account", "operation", "resource", "result"} {
		for _, blank := range blanks {
			values := map[string]string{
				"account": `"a"`, "operation": `"o"`, "resource": `"r"`, "result": `"ok"`,
			}
			values[field] = blank.value
			body := `{"account":` + values["account"] +
				`,"operation":` + values["operation"] +
				`,"resource":` + values["resource"] +
				`,"result":` + values["result"] +
				`,"occurred_at":"2026-10-05T00:00:00Z"}`
			wantInvalidInput(t, router, body)
		}
	}

	// The rejections changed nothing: exactly the seed record persists,
	// with its original text and hash.
	events := readFullLedger(t, path)
	if len(events) != 1 {
		t.Fatalf("persisted %d events after rejections, want 1", len(events))
	}
	if events[0].account != "alice" || events[0].operation != "login" ||
		events[0].resource != "console" || events[0].result != "ok" ||
		events[0].occurredAt != "2026-10-05T00:30:00Z" {
		t.Fatalf("stored seed = %+v, want the original record", events[0])
	}
	if events[0].seq != 1 || events[0].prevHash != genesis || events[0].hash != seedHash {
		t.Fatalf("stored chain = seq %d prev %s hash %s, want 1/genesis/%s",
			events[0].seq, events[0].prevHash, events[0].hash, seedHash)
	}

	// The next legal append takes the original next sequence number and
	// chains onto the unchanged tail.
	recorder = postEventBody(t, router,
		`{"account":"bob","operation":"read","resource":"report","result":"ok","occurred_at":"2026-10-05T01:00:00Z"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("append after rejections: status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	next := decodeCreated(t, recorder)
	wantNext := hashByPublicRule(2, "bob", "read", "report", "ok", "2026-10-05T01:00:00Z", seedHash)
	if next.Seq != 2 || next.PrevHash != seedHash || next.Hash != wantNext {
		t.Fatalf("next = seq %d prev %s hash %s, want 2/%s/%s",
			next.Seq, next.PrevHash, next.Hash, seedHash, wantNext)
	}
	events = readFullLedger(t, path)
	if len(events) != 2 || events[1].hash != wantNext || events[1].prevHash != seedHash {
		t.Fatalf("stored events after valid append = %+v", events)
	}
}

// TestPostEventSpecialCharsPersistAcrossReopen saves two records with
// special characters, reopens the same database file, and checks the text
// and hashes are unchanged and the next append chains onto the original
// tail record.
func TestPostEventSpecialCharsPersistAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st := openStoreAt(t, path)
	router := NewRouter(st)
	genesis := strings.Repeat("0", 64)

	// Record 1: control characters inside visible text, submitted escaped.
	rec1 := eventRequest{
		Account: "x\x00y", Operation: "li\tne", Resource: "res", Result: "ok",
		OccurredAt: "2026-10-05T08:30:00+08:00",
	}
	body1 := `{"account":"x\u0000y","operation":"li\tne","resource":"res","result":"ok",` +
		`"occurred_at":"2026-10-05T08:30:00+08:00"}`
	// Record 2: mixed text with a quote, a backslash, Chinese, a
	// supplementary plane character, angle brackets, & and U+2028,
	// submitted with direct characters where JSON allows them.
	rec2 := eventRequest{
		Account: "管理员😀", Operation: "op\x07", Resource: `a"b\c`, Result: "<&>\u2028",
		OccurredAt: "2026-10-05T00:00:00.25Z",
	}
	body2 := `{"account":"管理员😀","operation":"op\u0007","resource":"a\"b\\c",` +
		`"result":"<&>\u2028","occurred_at":"2026-10-05T00:00:00.25Z"}`

	recs := []struct {
		req         eventRequest
		body        string
		occurredUTC string
	}{
		{rec1, body1, "2026-10-05T00:30:00Z"},
		{rec2, body2, "2026-10-05T00:00:00.25Z"},
	}
	wantHashes := make([]string, 0, 3)
	prev := genesis
	for i, rec := range recs {
		recorder := postEventBody(t, router, rec.body)
		if recorder.Code != http.StatusCreated {
			t.Fatalf("record %d: status = %d (%s)", i+1, recorder.Code, recorder.Body.String())
		}
		got := decodeCreated(t, recorder)
		if got.Account != rec.req.Account || got.Operation != rec.req.Operation ||
			got.Resource != rec.req.Resource || got.Result != rec.req.Result {
			t.Fatalf("record %d: response fields = %+v, want %+v", i+1, got, rec.req)
		}
		if got.OccurredAt != rec.occurredUTC {
			t.Fatalf("record %d: occurred_at = %q, want %q", i+1, got.OccurredAt, rec.occurredUTC)
		}
		wantHash := hashByPublicRule(int64(i+1), rec.req.Account, rec.req.Operation,
			rec.req.Resource, rec.req.Result, rec.occurredUTC, prev)
		if got.Seq != int64(i+1) || got.PrevHash != prev || got.Hash != wantHash {
			t.Fatalf("record %d: seq = %d prev = %s hash = %s, want %d/%s/%s",
				i+1, got.Seq, got.PrevHash, got.Hash, i+1, prev, wantHash)
		}
		wantHashes = append(wantHashes, wantHash)
		prev = wantHash
	}
	tail := prev
	closeStore(t, st)

	// Reopen the same database: the two records keep their text and
	// hashes exactly.
	st = openStoreAt(t, path)
	events := readFullLedger(t, path)
	if len(events) != 2 {
		t.Fatalf("persisted %d events after reopen, want 2", len(events))
	}
	for i, rec := range recs {
		e := events[i]
		if e.account != rec.req.Account || e.operation != rec.req.Operation ||
			e.resource != rec.req.Resource || e.result != rec.req.Result ||
			e.occurredAt != rec.occurredUTC {
			t.Fatalf("row %d after reopen = %+v, want %+v", i, e, rec.req)
		}
		wantPrev := genesis
		if i > 0 {
			wantPrev = wantHashes[i-1]
		}
		if e.seq != int64(i+1) || e.prevHash != wantPrev || e.hash != wantHashes[i] {
			t.Fatalf("row %d chain after reopen = seq %d prev %s hash %s, want %d/%s/%s",
				i, e.seq, e.prevHash, e.hash, i+1, wantPrev, wantHashes[i])
		}
	}

	// The next append on the reopened database continues from the
	// original tail record.
	recorder := postEventBody(t, NewRouter(st),
		`{"account":"z\u0001z","operation":"verify","resource":"audit","result":"ok","occurred_at":"2026-10-05T01:00:00Z"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("append after reopen: status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	third := decodeCreated(t, recorder)
	wantThird := hashByPublicRule(3, "z\x01z", "verify", "audit", "ok", "2026-10-05T01:00:00Z", tail)
	if third.Seq != 3 || third.PrevHash != tail || third.Hash != wantThird {
		t.Fatalf("after reopen: seq = %d prev = %s hash = %s, want 3/%s/%s",
			third.Seq, third.PrevHash, third.Hash, tail, wantThird)
	}
	wantHashes = append(wantHashes, wantThird)
	closeStore(t, st)

	// A final independent read confirms all three records end to end.
	events = readFullLedger(t, path)
	if len(events) != 3 {
		t.Fatalf("persisted %d events at the end, want 3", len(events))
	}
	if events[2].account != "z\x01z" || events[2].seq != 3 ||
		events[2].prevHash != tail || events[2].hash != wantThird {
		t.Fatalf("final row = %+v, want the third record chained to %s", events[2], tail)
	}
}

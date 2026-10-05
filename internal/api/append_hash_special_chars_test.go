package api

// Regression tests for the POST /events audit hash over special characters:
//
//   - every control character U+0000..U+001F survives decoding, hashing and
//     persistence verbatim inside all four business text fields (account,
//     operation, resource, result), with the stored hash following the
//     public compact-JSON-array rule: double quote and backslash escaped,
//     control characters as lowercase six-character \u00xx escapes, every
//     other character as raw UTF-8 bytes;
//   - equivalent requests (short escapes, \uXXXX escapes, direct characters,
//     different key order, whitespace around the object, an equivalent
//     timezone offset) at the same chain position produce the same seq,
//     prev_hash and hash;
//   - mixed text with double quotes, backslashes, Chinese, supplementary
//     plane characters, <, >, & and U+2028/U+2029 survives a close/reopen
//     of the same database, and the chain keeps growing from the original
//     tail;
//   - raw unescaped control bytes inside strings and business text that
//     decodes to whitespace only are rejected with 400 invalid_audit_input
//     in the published error envelope, leaving the stored chain untouched.
//
// Expected hashes are derived from the documented rule in README.md over
// fixed test vectors; two of them are pinned as literal constants computed
// outside this codebase. Neither the service-returned hash nor the
// product's internal hash function is ever used as an expectation.
//
// Every U+2028/U+2029 and control character below is written as an explicit
// ASCII escape sequence, never as a literal character, so the test vectors
// survive editors and tooling that rewrite such characters.

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// Pinned hash vectors derived independently from the public rule in
// README.md (SHA-256 of the compact JSON array encoded as UTF-8).
const (
	// [1,"ac\u0000-00","op\u0000-00","res\u0000-00","ok\u0000-00",
	//  "2026-10-05T00:30:00Z","0000...0"]
	pinnedControlGenesisHash = "b48d361a50d5ae722563a06601d377eef98fa34e377a93ecc728f76c192d2c3d"
	// [1,"acct \"admin\" \\ 账号 😀 <>& <U+2028><U+2029> end",
	//  "op \"write\" \\ 操作 🚀 <&> x<U+2028>y<U+2029>z",
	//  "res \"db\" \\ 资源 𐀀 <>& <U+2028><U+2029>",
	//  "ok \"✓\" \\ 结果 🎉 <>& <U+2028><U+2029>",
	//  "2026-10-05T00:30:00.250Z","0000...0"]
	pinnedMixedSpecialHash = "336df05759035e334383abab4649b6392ba17cb678c8d7a553a9b9fd6264758a"
)

// lineSep and paraSep are U+2028 and U+2029, built from escapes so this
// source file never carries the literal characters.
const (
	lineSep = "\u2028"
	paraSep = "\u2029"
)

func TestPostEventControlCharactersRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st := openStoreAt(t, path)
	router := NewRouter(st)

	const occurredUTC = "2026-10-05T00:30:00Z"
	want := []ledgerRecord{}
	responseHash := map[int64]string{}
	prev := strings.Repeat("0", 64)
	for c := byte(0x00); c < 0x20; c++ {
		tag := fmt.Sprintf("%02x", c)
		// Visible text on both sides of the control character, so any
		// truncation, loss or rewrite of the byte changes the field.
		account := "ac" + string(rune(c)) + "-" + tag
		operation := "op" + string(rune(c)) + "-" + tag
		resource := "res" + string(rune(c)) + "-" + tag
		result := "ok" + string(rune(c)) + "-" + tag
		// JSON strings cannot carry raw control bytes; submit each one as
		// a \u00xx escape and expect the decoded character back verbatim.
		esc := `\u00` + tag
		body := fmt.Sprintf(`{"account":"ac%s-%s","operation":"op%s-%s","resource":"res%s-%s","result":"ok%s-%s","occurred_at":"2026-10-05T08:30:00+08:00"}`,
			esc, tag, esc, tag, esc, tag, esc, tag)

		recorder := postEventBody(t, router, body)
		if recorder.Code != http.StatusCreated {
			t.Fatalf("U+%04X: status = %d, want 201 (%s)", c, recorder.Code, recorder.Body.String())
		}
		got := decodeCreated(t, recorder)
		seq := int64(c) + 1
		if got.Seq != seq || got.PrevHash != prev {
			t.Fatalf("U+%04X: seq = %d prev_hash = %s, want %d chained to %s", c, got.Seq, got.PrevHash, seq, prev)
		}
		if got.Account != account || got.Operation != operation ||
			got.Resource != resource || got.Result != result {
			t.Fatalf("U+%04X: response fields = %+v, want verbatim %q %q %q %q",
				c, got, account, operation, resource, result)
		}
		if got.OccurredAt != occurredUTC {
			t.Fatalf("U+%04X: occurred_at = %q, want %q", c, got.OccurredAt, occurredUTC)
		}
		if want := wantHash(seq, account, operation, resource, result, occurredUTC, prev); got.Hash != want {
			t.Fatalf("U+%04X: hash = %s, want %s per the public rule", c, got.Hash, want)
		}
		prev = got.Hash
		responseHash[seq] = got.Hash
		want = append(want, ledgerRecord{account, operation, resource, result, occurredUTC})
	}
	// The genesis record of the sweep is pinned to a fixed vector derived
	// outside this codebase, so a drift in both implementations cannot
	// cancel out.
	if responseHash[1] != pinnedControlGenesisHash {
		t.Fatalf("seq 1 hash = %s, want pinned vector %s", responseHash[1], pinnedControlGenesisHash)
	}

	closeStore(t, st)
	// Every field of every row persisted in SQLite matches the decoded
	// text, and every stored hash recomputes under the public rule.
	verifyLedger(t, path, want, responseHash)
}

func TestPostEventEquivalentEncodingsShareHash(t *testing.T) {
	// One logical event carrying a double quote, a backslash, control
	// characters, Chinese, supplementary-plane characters and U+2028/U+2029.
	account := "eq \"A\" \\ 账号 😀 " + lineSep + paraSep
	operation := "op\n\t\x01 读写"
	resource := "res \\ \"Q\" 资源 🚀"
	result := "ok \"✓\" \\ 结果 𐀀 " + lineSep
	const occurredUTC = "2026-10-05T00:30:00Z"
	genesis := strings.Repeat("0", 64)
	want := wantHash(1, account, operation, resource, result, occurredUTC, genesis)

	variants := map[string]string{
		// Short escapes for the control characters that have them, \uXXXX
		// (with surrogate pairs) for everything non-ASCII.
		"short and unicode escapes": `{"account":"eq \"A\" \\ \u8d26\u53f7 \ud83d\ude00 \u2028\u2029","operation":"op\n\t\u0001 \u8bfb\u5199","resource":"res \\ \"Q\" \u8d44\u6e90 \ud83d\ude80","result":"ok \"\u2713\" \\ \u7ed3\u679c \ud800\udc00 \u2028","occurred_at":"2026-10-05T08:30:00+08:00"}`,
		// Control characters as \uXXXX escapes, multibyte characters direct.
		"unicode escapes and direct multibyte": `{"account":"eq \"A\" \\ 账号 😀 \u2028\u2029","operation":"op\u000a\u0009\u0001 读写","resource":"res \\ \"Q\" 资源 🚀","result":"ok \"✓\" \\ 结果 𐀀 \u2028","occurred_at":"2026-10-05T08:30:00+08:00"}`,
		// Direct characters wherever JSON allows them (U+2028/U+2029
		// included), short escapes for the control characters.
		"direct characters": `{"account":"eq \"A\" \\ 账号 😀 ` + lineSep + paraSep + `","operation":"op\n\t\u0001 读写","resource":"res \\ \"Q\" 资源 🚀","result":"ok \"✓\" \\ 结果 𐀀 ` + lineSep + `","occurred_at":"2026-10-05T08:30:00+08:00"}`,
		// Reordered keys, whitespace around the object and an equivalent
		// timezone offset that normalizes to the same UTC instant.
		"key order and whitespace": "{\n" +
			`  "occurred_at" : "2026-10-04T18:30:00-06:00" ,` + "\n" +
			`  "result" : "ok \"✓\" \\ 结果 𐀀 ` + lineSep + `" ,` + "\n" +
			`  "resource" : "res \\ \"Q\" 资源 🚀" ,` + "\n" +
			`  "operation" : "op\n\t\u0001 读写" ,` + "\n" +
			`  "account" : "eq \"A\" \\ 账号 😀 \u2028\u2029"` + "\n" +
			"}\n",
	}
	for name, body := range variants {
		t.Run(name, func(t *testing.T) {
			// Each variant appends at the same chain position: seq 1 on a
			// fresh ledger, chained to the genesis prev_hash.
			router := NewRouter(openTestStore(t))
			recorder := postEventBody(t, router, body)
			if recorder.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201 (%s)", recorder.Code, recorder.Body.String())
			}
			got := decodeCreated(t, recorder)
			if got.Seq != 1 || got.PrevHash != genesis {
				t.Fatalf("seq = %d prev_hash = %s, want 1 chained to genesis", got.Seq, got.PrevHash)
			}
			if got.Account != account || got.Operation != operation ||
				got.Resource != resource || got.Result != result {
				t.Fatalf("decoded fields = %+v, want %q %q %q %q", got, account, operation, resource, result)
			}
			if got.OccurredAt != occurredUTC {
				t.Fatalf("occurred_at = %q, want %q", got.OccurredAt, occurredUTC)
			}
			if got.Hash != want {
				t.Fatalf("hash = %s, want %s: equivalent encodings must hash identically", got.Hash, want)
			}
		})
	}
}

func TestPostEventMixedSpecialTextSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st := openStoreAt(t, path)
	router := NewRouter(st)

	// Double quotes, backslashes, Chinese, supplementary-plane characters,
	// <, >, & and U+2028/U+2029 in every business field.
	first := eventFixture{
		req: eventRequest{
			Account:    "acct \"admin\" \\ 账号 😀 <>& " + lineSep + paraSep + " end",
			Operation:  "op \"write\" \\ 操作 🚀 <&> x" + lineSep + "y" + paraSep + "z",
			Resource:   "res \"db\" \\ 资源 𐀀 <>& " + lineSep + paraSep,
			Result:     "ok \"✓\" \\ 结果 🎉 <>& " + lineSep + paraSep,
			OccurredAt: "2026-10-05T08:30:00.250+08:00",
		},
		occurredUTC: "2026-10-05T00:30:00.250Z",
	}
	second := eventFixture{
		req: eventRequest{
			Account:    "乙 \"二\" \\ 𐀀😀 <&> " + paraSep,
			Operation:  "del \\ \"🚀\" 操作 " + lineSep,
			Resource:   "res-2 <>& \\ \"q\" 资源 " + paraSep + lineSep,
			Result:     "denied \"✗\" \\ 结果 🎉 " + lineSep,
			OccurredAt: "2026-10-05T02:30:00-06:00",
		},
		occurredUTC: "2026-10-05T08:30:00Z",
	}
	genesis := strings.Repeat("0", 64)

	post := func(fx eventFixture, seq int64, prev string) createdEvent {
		t.Helper()
		recorder := postStructuredEvent(t, router, fx.req)
		if recorder.Code != http.StatusCreated {
			t.Fatalf("seq %d: status = %d, want 201 (%s)", seq, recorder.Code, recorder.Body.String())
		}
		got := decodeCreated(t, recorder)
		if got.Seq != seq || got.PrevHash != prev {
			t.Fatalf("seq %d: got seq = %d prev_hash = %s, want %d chained to %s",
				seq, got.Seq, got.PrevHash, seq, prev)
		}
		if got.Account != fx.req.Account || got.Operation != fx.req.Operation ||
			got.Resource != fx.req.Resource || got.Result != fx.req.Result {
			t.Fatalf("seq %d: response fields = %+v, want verbatim %+v", seq, got, fx.req)
		}
		if got.OccurredAt != fx.occurredUTC {
			t.Fatalf("seq %d: occurred_at = %q, want %q", seq, got.OccurredAt, fx.occurredUTC)
		}
		if want := wantHash(seq, fx.req.Account, fx.req.Operation, fx.req.Resource,
			fx.req.Result, fx.occurredUTC, prev); got.Hash != want {
			t.Fatalf("seq %d: hash = %s, want %s per the public rule", seq, got.Hash, want)
		}
		return got
	}

	created1 := post(first, 1, genesis)
	// The first mixed record is pinned to a fixed, externally derived vector.
	if created1.Hash != pinnedMixedSpecialHash {
		t.Fatalf("seq 1 hash = %s, want pinned vector %s", created1.Hash, pinnedMixedSpecialHash)
	}
	created2 := post(second, 2, created1.Hash)

	want := []ledgerRecord{recordOf(first), recordOf(second)}
	responseHash := map[int64]string{1: created1.Hash, 2: created2.Hash}

	// Close, then read the file through an independent connection: both
	// special-text records are persisted byte-for-byte with their hashes.
	closeStore(t, st)
	verifyLedger(t, path, want, responseHash)

	// Reopen the same database: text and hashes are unchanged, and the next
	// append continues from the original tail.
	st = openStoreAt(t, path)
	router = NewRouter(st)
	verifyLedger(t, path, want, responseHash)

	third := eventFixture{
		req: eventRequest{
			Account: "carol", Operation: "verify", Resource: "audit-log", Result: "ok",
			OccurredAt: "2026-10-05T09:00:00.5Z",
		},
		occurredUTC: "2026-10-05T09:00:00.5Z",
	}
	created3 := post(third, 3, created2.Hash)
	want = append(want, recordOf(third))
	responseHash[3] = created3.Hash
	closeStore(t, st)

	// The full three-record chain verifies after the final close.
	verifyLedger(t, path, want, responseHash)
}

func TestPostEventRejectsRawControlBytesAndBlankText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st := openStoreAt(t, path)
	router := NewRouter(st)

	// Seed one valid record and snapshot the chain tail.
	seedRecorder := postEventBody(t, router, validEventBody())
	if seedRecorder.Code != http.StatusCreated {
		t.Fatalf("seed: status = %d (%s)", seedRecorder.Code, seedRecorder.Body.String())
	}
	seed := decodeCreated(t, seedRecorder)
	if seed.Seq != 1 || seed.PrevHash != strings.Repeat("0", 64) {
		t.Fatalf("seed = %+v, want seq 1 chained to genesis", seed)
	}

	// Raw, unescaped control bytes inside a JSON string are not valid JSON
	// and must be rejected, not sanitized.
	rawControls := map[string]string{
		"NUL": "\x00",
		"SOH": "\x01",
		"TAB": "\t",
		"LF":  "\n",
		"US":  "\x1f",
	}
	for name, raw := range rawControls {
		t.Run("raw control byte "+name, func(t *testing.T) {
			body := `{"account":"a` + raw + `x","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z"}`
			wantInvalidInput(t, router, body)
		})
	}

	// Business text that decodes to whitespace only (here: tab, vertical
	// tab, form feed, carriage return and space) is rejected in each of the
	// four business fields.
	const blank = `"\u0009 \u000b\u000c\u000d "`
	values := map[string]string{"account": `"a"`, "operation": `"o"`, "resource": `"r"`, "result": `"ok"`}
	for _, field := range []string{"account", "operation", "resource", "result"} {
		t.Run("whitespace only "+field, func(t *testing.T) {
			v := map[string]string{}
			for k, val := range values {
				v[k] = val
			}
			v[field] = blank
			body := fmt.Sprintf(`{"account":%s,"operation":%s,"resource":%s,"result":%s,"occurred_at":"2026-10-05T00:00:00Z"}`,
				v["account"], v["operation"], v["resource"], v["result"])
			wantInvalidInput(t, router, body)
		})
	}

	// The rejections did not damage the service or the stored chain.
	if recorder := getHealthz(t, router); recorder.Code != http.StatusOK {
		t.Fatalf("healthz after rejections: status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	events := readFullLedger(t, path)
	if len(events) != 1 || events[0].hash != seed.Hash || events[0].account != "alice" {
		t.Fatalf("stored events after rejections = %+v, want only the seed record", events)
	}

	// The next legal append uses the original next sequence number and
	// chains onto the untouched tail.
	follow := eventFixture{
		req: eventRequest{
			Account: "carol", Operation: "retry", Resource: "console", Result: "ok",
			OccurredAt: "2026-10-05T04:00:00+04:00",
		},
		occurredUTC: "2026-10-05T00:00:00Z",
	}
	recorder := postStructuredEvent(t, router, follow.req)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("append after rejections: status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	got := decodeCreated(t, recorder)
	if got.Seq != 2 || got.PrevHash != seed.Hash {
		t.Fatalf("after rejections: seq = %d prev_hash = %s, want 2 chained to %s",
			got.Seq, got.PrevHash, seed.Hash)
	}
	if got.Hash != wantHash(2, follow.req.Account, follow.req.Operation, follow.req.Resource,
		follow.req.Result, follow.occurredUTC, seed.Hash) {
		t.Fatalf("after rejections: hash = %s does not follow the public rule", got.Hash)
	}
	closeStore(t, st)

	verifyLedger(t, path,
		[]ledgerRecord{
			{account: "alice", operation: "login", resource: "console", result: "ok", occurredAt: "2026-10-05T00:30:00Z"},
			recordOf(follow),
		},
		map[int64]string{1: seed.Hash, 2: got.Hash})
}

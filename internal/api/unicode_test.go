package api

import (
	"database/sql"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xjeey8iust/sen-audit-ledger/internal/store"
)

// unicodeEventBody wraps a JSON-encoded account value in an otherwise valid
// event body. accountJSON is the raw JSON text of the value, e.g. `"alice"`,
// `"x\uD83D"` or a string carrying invalid bytes.
func unicodeEventBody(accountJSON string) string {
	return `{"account":` + accountJSON + `,"operation":"op","resource":"res","result":"ok","occurred_at":"2026-10-05T00:00:00Z"}`
}

// wantInvalidUnicode posts a body and asserts the strict-validation rejection
// shape: 400 invalid_audit_input, only a top-level error object, and no
// internals leaked in the message.
func wantInvalidUnicode(t *testing.T, router http.Handler, body string) {
	t.Helper()
	recorder := postEventBody(t, router, body)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d (%s)", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
	decoded := decodeEvent(t, recorder)
	if len(decoded) != 1 {
		t.Fatalf("response keys = %v, want only the error object", decoded)
	}
	errObj, ok := decoded["error"].(map[string]any)
	if !ok {
		t.Fatalf("error = %v, want an object", decoded["error"])
	}
	if errObj["code"] != "invalid_audit_input" {
		t.Fatalf("code = %v, want invalid_audit_input", errObj["code"])
	}
	if message, ok := errObj["message"].(string); !ok || message == "" {
		t.Fatalf("message = %v, want a non-empty string", errObj["message"])
	}
	leaks := []string{"sql", "SELECT", "INSERT", "goroutine", ".go", "/", "\\"}
	for _, leak := range leaks {
		if strings.Contains(recorder.Body.String(), leak) {
			t.Fatalf("response leaks internals (%q): %s", leak, recorder.Body.String())
		}
	}
}

func TestPostEventRejectsInvalidUnicode(t *testing.T) {
	router := NewRouter(openTestStore(t))

	cases := map[string]string{
		"invalid byte in value":          unicodeEventBody("\"a\xff\""),
		"invalid byte outside strings":   "{\xff" + unicodeEventBody("\"a\"")[1:],
		"truncated three-byte char":      unicodeEventBody("\"a\xe4\xb8\""),
		"truncated two-byte char":        unicodeEventBody("\"a\xc3\""),
		"bad continuation byte":          unicodeEventBody("\"a\xe4\xff\""),
		"truncated byte at end":          unicodeEventBody("\"a\xe4\""),
		"utf8 encoded surrogate":         unicodeEventBody("\"a\xed\xa0\xbd\xed\xb8\x80\""),
		"lone utf8 encoded surrogate":    unicodeEventBody("\"a\xed\xa0\x80\""),
		"overlong encoding":              unicodeEventBody("\"a\xc0\xaf\""),
		"codepoint above 10ffff":         unicodeEventBody("\"a\xf4\x90\x80\x80\""),
		"lone high surrogate":            unicodeEventBody(`"x\uD83D"`),
		"lone low surrogate":             unicodeEventBody(`"x\uDE00"`),
		"reversed surrogate pair":        unicodeEventBody(`"\uDE00\uD83D"`),
		"two high surrogates":            unicodeEventBody(`"\uD83D\uD83D\uDE00"`),
		"two low surrogates":             unicodeEventBody(`"\uDE00\uDE00"`),
		"pair split by literal":          unicodeEventBody(`"\uD83Da\uDE00"`),
		"pair split by escape":           unicodeEventBody(`"\uD83D\n\uDE00"`),
		"pair broken by escaped slash":   unicodeEventBody(`"\\uD83D\uDE00"`),
		"truncated unicode escape":       unicodeEventBody(`"\uD83"`),
		"non-hex unicode escape":         unicodeEventBody(`"\uGGGG"`),
		"surrogate in field name":        `{"x\uD83D":1,"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z"}`,
		"invalid byte in field name":     `{"x` + "\xff" + `":1,"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z"}`,
		"surrogate split across strings": `{"account":"x\uD83D","operation":"o\uDE00p","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			wantInvalidUnicode(t, router, body)
		})
	}
}

func TestPostEventAcceptsValidUnicode(t *testing.T) {
	cases := map[string]struct {
		accountJSON string // raw JSON value text
		want        string // decoded account content
	}{
		"direct chinese":               {`"张三"`, "张三"},
		"direct supplementary char":    {`"a😀b"`, "a😀b"},
		"escaped surrogate pair":       {`"a\uD83D\uDE00b"`, "a😀b"},
		"uppercase hex pair":           {`"a\uD83d\uDe00b"`, "a😀b"},
		"direct replacement char":      {"\"a�b\"", "a�b"},
		"escaped replacement char":     {`"a\uFFFDb"`, "a�b"},
		"escaped backslash is literal": {`"\\uD83D"`, `\uD83D`},
		"two escaped backslashes":      {`"\\uD83D\\uDE00"`, `\uD83D\uDE00`},
		"basic unicode escape":         {`"\u0041"`, "A"},
		"whitespace kept":              {`"  张三  "`, "  张三  "},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			router := NewRouter(openTestStore(t))
			recorder := postEventBody(t, router, unicodeEventBody(tc.accountJSON))
			if recorder.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201 (%s)", recorder.Code, recorder.Body.String())
			}
			if event := decodeEvent(t, recorder); event["account"] != tc.want {
				t.Fatalf("account = %q, want %q", event["account"], tc.want)
			}
		})
	}
}

func TestPostEventSurrogatePairMatchesDirectCharacter(t *testing.T) {
	// A correctly paired \uD83D\uDE00 must decode to the same field content
	// and hash as the character submitted directly.
	direct := unicodeEventBody(`"u😀"`)
	escaped := unicodeEventBody(`"u\uD83D\uDE00"`)

	routerA := NewRouter(openTestStore(t))
	routerB := NewRouter(openTestStore(t))
	a := decodeEvent(t, postEventBody(t, routerA, direct))
	b := decodeEvent(t, postEventBody(t, routerB, escaped))

	if a["account"] != "u😀" || b["account"] != "u😀" {
		t.Fatalf("accounts = %q and %q, want u😀", a["account"], b["account"])
	}
	if a["account"] != b["account"] {
		t.Fatalf("escaped pair decoded to %q, direct gave %q", b["account"], a["account"])
	}
	if a["seq"] != b["seq"] || a["prev_hash"] != b["prev_hash"] || a["hash"] != b["hash"] {
		t.Fatalf("pair/direct records differ: %v vs %v", a, b)
	}
	want := wantHash(1, "u😀", "op", "res", "ok", "2026-10-05T00:00:00Z", strings.Repeat("0", 64))
	if a["hash"] != want {
		t.Fatalf("hash = %v, want %v", a["hash"], want)
	}

	// A real U+FFFD, direct or \uFFFD-escaped, is legitimate content and the
	// two encodings hash identically.
	routerC := NewRouter(openTestStore(t))
	routerD := NewRouter(openTestStore(t))
	c := decodeEvent(t, postEventBody(t, routerC, unicodeEventBody("\"a�\"")))
	d := decodeEvent(t, postEventBody(t, routerD, unicodeEventBody(`"a\uFFFD"`)))
	if c["account"] != "a�" || d["account"] != "a�" {
		t.Fatalf("accounts = %q and %q, want a�", c["account"], d["account"])
	}
	if c["hash"] != d["hash"] {
		t.Fatalf("U+FFFD encodings hash differently: %v vs %v", c["hash"], d["hash"])
	}
}

func TestPostEventPersistsUnicodeContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	router := NewRouter(st)

	body := func(account string) string {
		return `{"account":` + account + `,"operation":"op","resource":"r😀","result":"ok","occurred_at":"2026-10-05T00:00:00Z"}`
	}
	inputs := []string{
		`"张三"`,
		`"a\uD83D\uDE00b"`,
		"\"a�b\"",
		`"\\uD83D"`,
	}
	wants := []string{"张三", "a😀b", "a�b", `\uD83D`}
	for _, in := range inputs {
		if recorder := postEventBody(t, router, body(in)); recorder.Code != http.StatusCreated {
			t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT account, resource FROM events ORDER BY seq")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var gotAccounts []string
	for rows.Next() {
		var account, resource string
		if err := rows.Scan(&account, &resource); err != nil {
			t.Fatalf("scan: %v", err)
		}
		gotAccounts = append(gotAccounts, account)
		if resource != "r😀" {
			t.Fatalf("resource = %q, want r😀", resource)
		}
	}
	if len(gotAccounts) != len(wants) {
		t.Fatalf("stored %d rows, want %d", len(gotAccounts), len(wants))
	}
	for i := range wants {
		if gotAccounts[i] != wants[i] {
			t.Fatalf("row %d account = %q, want %q", i, gotAccounts[i], wants[i])
		}
	}
}

func TestPostEventInvalidUnicodePrecedesSeqChecksAndStorage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	router := NewRouter(st)

	// Seed one valid record so seq 1 exists and seq 9 is a gap.
	seed := decodeEvent(t, postEventBody(t, router, validEventBody()))

	invalidWithSeq := func(seq string) string {
		return `{"account":"a\xff","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z","seq":` + seq + `}`
	}
	// Existing seq must still be 400 invalid_audit_input, never 409.
	wantInvalidUnicode(t, router, invalidWithSeq("1"))
	// Gap seq is rejected by the Unicode gate as well.
	wantInvalidUnicode(t, router, invalidWithSeq("9"))
	// Lone surrogate without seq, same outcome.
	wantInvalidUnicode(t, router, unicodeEventBody(`"x\uD83D"`))

	// Record count and chain tail are unchanged.
	stored := readStoredEvents(t, path)
	if len(stored) != 1 {
		t.Fatalf("stored %d events, want 1", len(stored))
	}
	if stored[0].seq != 1 || stored[0].hash != seed["hash"] {
		t.Fatalf("chain tail moved after rejections: %+v", stored[0])
	}

	// The next valid append continues from the original tail as seq 2.
	next := decodeEvent(t, postEventBody(t, router, validEventBody()))
	if next["seq"] != float64(2) || next["prev_hash"] != seed["hash"] {
		t.Fatalf("next = %v, want seq 2 on top of %v", next, seed["hash"])
	}
	st.Close()

	// Restart continues the same chain.
	st, err = store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st.Close()
	after := decodeEvent(t, postEventBody(t, NewRouter(st), validEventBody()))
	if after["seq"] != float64(3) || after["prev_hash"] != next["hash"] {
		t.Fatalf("after reopen = %v, want seq 3 chained to %v", after, next["hash"])
	}
}

func TestHasValidUnicodeEscapes(t *testing.T) {
	cases := map[string]bool{
		// Valid.
		`{"a":"\uD83D\uDE00"}`:             true,
		`{"a":"x\uD83D\uDE00y"}`:           true,
		`{"a":"\uD83D\uDE00\uD83D\uDE00"}`: true,
		`{"a":"\uD83d\uDe00"}`:             true,
		`{"a":"\u00e9"}`:                   true,
		`{"a":"\\uD83D"}`:                  true,
		`{"a":"\\uD83D\\uDE00"}`:           true,
		`{"\uD83D\uDE00":1}`:               true,
		`{"a":"x","b":"y\uD83C\uDF63z"}`:   true,
		`[1,2,3]`:                          true,
		// Invalid.
		`{"a":"\uD83D"}`:              false,
		`{"a":"\uDE00"}`:              false,
		`{"a":"\uDE00\uD83D"}`:        false,
		`{"a":"\uD83D\uD83D\uDE00"}`:  false,
		`{"a":"\uDE00\uDE00"}`:        false,
		`{"a":"\uD83Da\uDE00"}`:       false,
		`{"a":"\uD83D\n\uDE00"}`:      false,
		`{"a":"\uD83D","b":"\uDE00"}`: false,
		`{"a":"\\uD83D\uDE00"}`:       false,
		`{"a":"\uD83"}`:               false,
		`{"a":"\uGGGG"}`:              false,
		`{"x\uD83D":1}`:               false,
		`{"a":"\uD83D\uGGGG"}`:        false,
		`{"a":"\uD83D\uDE0"}`:         false,
	}
	for body, want := range cases {
		if got := hasValidUnicodeEscapes([]byte(body)); got != want {
			t.Errorf("hasValidUnicodeEscapes(%s) = %v, want %v", body, got, want)
		}
	}
}

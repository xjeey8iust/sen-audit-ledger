package api

// Regression tests for request-body reading at POST /events:
//
//   - the 1 MiB body limit is enforced on raw UTF-8 bytes: 1048575- and
//     1048576-byte bodies are accepted, a 1048577-byte body is rejected with
//     400 invalid_audit_input even when the extra byte is legal trailing
//     whitespace, and multi-byte text (direct Chinese, supplementary-plane
//     characters, \uXXXX escapes) counts by bytes, not by characters;
//   - a transport-level read failure is the same 400 invalid_audit_input
//     whether it happens before any data, mid-document, or after a complete
//     valid object — even one carrying an already-existing seq, where the
//     read failure still wins over the 409 conflict — and never persists
//     the event;
//   - failed requests consume no sequence number and move no chain tail,
//     the database holds only the successful requests, a close/reopen keeps
//     records and tail intact, and /healthz stays 200 throughout.
//
// Every expectation goes through the public HTTP entry and the rows actually
// persisted; hashes are recomputed from the public SHA-256 rule in README.md,
// never by calling the product's parsing internals.

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// The request-body limit these tests pin, in raw bytes.
const (
	bodyLimit      = 1 << 20 // 1048576
	bodyLimitUnder = bodyLimit - 1
	bodyLimitOver  = bodyLimit + 1
)

// Sized bodies all carry the same occurred_at, supplied with an offset so
// the UTC normalization rule is exercised on every accepted request.
const (
	sizedBodyOccurredAt  = "2026-10-05T08:30:00+08:00"
	sizedBodyOccurredUTC = "2026-10-05T00:30:00Z"
)

// errInjectedRead simulates a transport-level failure while the body is read.
var errInjectedRead = errors.New("injected transport read failure")

// flakyReader yields prefix and then fails every Read with errInjectedRead,
// simulating a connection that dies mid-body.
type flakyReader struct {
	prefix []byte
}

func (r *flakyReader) Read(p []byte) (int, error) {
	if len(r.prefix) == 0 {
		return 0, errInjectedRead
	}
	n := copy(p, r.prefix)
	r.prefix = r.prefix[n:]
	return n, nil
}

// postEventReader sends one POST /events request whose body comes from an
// arbitrary reader, so tests control exactly how the read ends.
func postEventReader(t *testing.T, router http.Handler, reader io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/events", reader)
	router.ServeHTTP(recorder, request)
	return recorder
}

// exactSizeEventBody builds a syntactically valid event document of exactly
// total bytes: the resource field absorbs whatever padding is needed, and
// lead/trail contribute legal surrounding whitespace (which counts toward
// the limit). It returns the body and the resource value inside it.
func exactSizeEventBody(t *testing.T, accountJSON, lead, trail string, total int, pad func(t *testing.T, width int) string) (string, string) {
	t.Helper()
	prefix := lead + `{"account":` + accountJSON + `,"operation":"o","resource":"`
	suffix := `","result":"ok","occurred_at":"` + sizedBodyOccurredAt + `"}` + trail
	width := total - len(prefix) - len(suffix)
	if width < 0 {
		t.Fatalf("fixed fields are %d bytes, already over the %d-byte target", len(prefix)+len(suffix), total)
	}
	resource := pad(t, width)
	if len(resource) != width {
		t.Fatalf("padding = %d bytes, want %d", len(resource), width)
	}
	body := prefix + resource + suffix
	if len(body) != total {
		t.Fatalf("body = %d bytes, want %d", len(body), total)
	}
	return body, resource
}

// asciiPad fills width bytes with a single-byte character.
func asciiPad(t *testing.T, width int) string {
	t.Helper()
	return strings.Repeat("x", width)
}

// multibytePad fills exactly width bytes mostly with 3-byte Chinese
// characters (plus one 2-byte character or one ASCII byte to land exactly),
// so the rune count stays far below the byte count.
func multibytePad(t *testing.T, width int) string {
	t.Helper()
	var b strings.Builder
	for width >= 3 {
		b.WriteString("审")
		width -= 3
	}
	switch width {
	case 2:
		b.WriteString("é") // 2 bytes, 1 rune
	case 1:
		b.WriteString("x")
	}
	return b.String()
}

// mustPostSizedBody posts one exact-size body that must be accepted: the
// decoded business text comes back verbatim, occurred_at follows the UTC
// rule, and the hash matches the public SHA-256 rule.
func mustPostSizedBody(t *testing.T, router http.Handler, body string, wantSeq int64, wantPrevHash, wantAccount, wantResource string) createdEvent {
	t.Helper()
	recorder := postEventBody(t, router, body)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", recorder.Code, recorder.Body.String())
	}
	event := decodeCreated(t, recorder)
	if event.Seq != wantSeq || event.PrevHash != wantPrevHash {
		t.Fatalf("seq = %d prev_hash = %s, want %d and %s", event.Seq, event.PrevHash, wantSeq, wantPrevHash)
	}
	if event.Account != wantAccount || event.Operation != "o" || event.Result != "ok" {
		t.Fatalf("business text = account %q operation %q result %q, want %q/o/ok decoded verbatim",
			event.Account, event.Operation, event.Result, wantAccount)
	}
	if event.Resource != wantResource {
		t.Fatalf("resource = %d bytes, want the %d-byte padding preserved verbatim", len(event.Resource), len(wantResource))
	}
	if event.OccurredAt != sizedBodyOccurredUTC {
		t.Fatalf("occurred_at = %q, want %q", event.OccurredAt, sizedBodyOccurredUTC)
	}
	if want := wantHash(wantSeq, wantAccount, "o", wantResource, "ok", sizedBodyOccurredUTC, wantPrevHash); event.Hash != want {
		t.Fatalf("hash = %s, want %s per the public rule", event.Hash, want)
	}
	return event
}

// verifyFailedAppendLeavesChainUntouched runs the full failed-append
// scenario on a fresh database: seed one committed record, fire failOnce
// (the oversized or unreadable request), then prove the failure consumed no
// sequence number and moved no chain tail — the next valid append is seq 2
// chained to the seed, only successful requests were persisted, and a
// close/reopen keeps the records and the tail intact. ghostAccounts are the
// accounts used inside the failing request, which must never be persisted.
func verifyFailedAppendLeavesChainUntouched(t *testing.T, failOnce func(t *testing.T, router http.Handler) *httptest.ResponseRecorder, ghostAccounts ...string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "service.db")
	st := openStoreAt(t, path)
	router := NewRouter(st)

	// Seed one committed record.
	seedRec := postEventBody(t, router, validEventBody())
	if seedRec.Code != http.StatusCreated {
		t.Fatalf("seed: status = %d (%s)", seedRec.Code, seedRec.Body.String())
	}
	seed := decodeCreated(t, seedRec)
	if seed.Seq != 1 || seed.PrevHash != strings.Repeat("0", 64) {
		t.Fatalf("seed = %+v, want seq 1 chained to genesis", seed)
	}

	// The failing request: 400 invalid_audit_input in the published
	// envelope, with no trace of the underlying read error.
	recorder := failOnce(t, router)
	assertErrorEnvelope(t, recorder, http.StatusBadRequest, "invalid_audit_input")
	if strings.Contains(recorder.Body.String(), errInjectedRead.Error()) {
		t.Fatalf("response leaks the read error text: %s", recorder.Body.String())
	}

	// Storage stays available while a request body cannot be read.
	if health := getHealthz(t, router); health.Code != http.StatusOK {
		t.Fatalf("healthz after failed request: status = %d (%s)", health.Code, health.Body.String())
	}

	// The failure consumed no sequence number and moved no chain tail.
	nextRec := postEventBody(t, router, validEventBody())
	if nextRec.Code != http.StatusCreated {
		t.Fatalf("append after failure: status = %d (%s)", nextRec.Code, nextRec.Body.String())
	}
	next := decodeCreated(t, nextRec)
	if next.Seq != 2 || next.PrevHash != seed.Hash {
		t.Fatalf("after failure: seq = %d prev_hash = %s, want 2 and %s", next.Seq, next.PrevHash, seed.Hash)
	}

	seedRecord := ledgerRecord{account: "alice", operation: "login", resource: "console", result: "ok", occurredAt: "2026-10-05T00:30:00Z"}
	want := []ledgerRecord{seedRecord, seedRecord}
	responseHash := map[int64]string{1: seed.Hash, 2: next.Hash}
	ghosts := map[string]bool{}
	for _, account := range ghostAccounts {
		ghosts[account] = true
	}

	// Only the successful requests were persisted.
	events := verifyLedger(t, path, want, responseHash)
	assertAccountsAbsent(t, events, ghosts)

	// Close and reopen: records and chain tail are intact, and the sequence
	// continues from the same tail.
	closeStore(t, st)
	st = openStoreAt(t, path)
	thirdRec := postEventBody(t, NewRouter(st), validEventBody())
	if thirdRec.Code != http.StatusCreated {
		t.Fatalf("append after reopen: status = %d (%s)", thirdRec.Code, thirdRec.Body.String())
	}
	third := decodeCreated(t, thirdRec)
	if third.Seq != 3 || third.PrevHash != next.Hash {
		t.Fatalf("after reopen: seq = %d prev_hash = %s, want 3 and %s", third.Seq, third.PrevHash, next.Hash)
	}
	closeStore(t, st)

	want = append(want, seedRecord)
	responseHash[3] = third.Hash
	events = verifyLedger(t, path, want, responseHash)
	assertAccountsAbsent(t, events, ghosts)
}

func TestPostEventBodySizeBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st := openStoreAt(t, path)
	router := NewRouter(st)
	genesis := strings.Repeat("0", 64)

	// 1 MiB - 1 byte: direct Chinese and a direct supplementary-plane
	// character in the account field.
	underBody, underResource := exactSizeEventBody(t, `"账号😀"`, "", "", bodyLimitUnder, asciiPad)
	first := mustPostSizedBody(t, router, underBody, 1, genesis, "账号😀", underResource)

	// Exactly 1 MiB: the same text as JSON Unicode escapes, wrapped in
	// legal leading/trailing whitespace that counts toward the total.
	exactBody, exactResource := exactSizeEventBody(t, `"\u8d26\u53f7\uD83D\uDE00"`, "\n\t ", " \r\n", bodyLimit, asciiPad)
	second := mustPostSizedBody(t, router, exactBody, 2, first.Hash, "账号😀", exactResource)

	want := []ledgerRecord{
		{account: "账号😀", operation: "o", resource: underResource, result: "ok", occurredAt: sizedBodyOccurredUTC},
		{account: "账号😀", operation: "o", resource: exactResource, result: "ok", occurredAt: sizedBodyOccurredUTC},
	}
	responseHash := map[int64]string{1: first.Hash, 2: second.Hash}

	// Both boundary bodies were persisted verbatim.
	closeStore(t, st)
	verifyLedger(t, path, want, responseHash)

	// After a close/reopen the chain continues from the same tail.
	st = openStoreAt(t, path)
	thirdRec := postEventBody(t, NewRouter(st), validEventBody())
	if thirdRec.Code != http.StatusCreated {
		t.Fatalf("append after reopen: status = %d (%s)", thirdRec.Code, thirdRec.Body.String())
	}
	third := decodeCreated(t, thirdRec)
	if third.Seq != 3 || third.PrevHash != second.Hash {
		t.Fatalf("after reopen: seq = %d prev_hash = %s, want 3 and %s", third.Seq, third.PrevHash, second.Hash)
	}
	closeStore(t, st)

	want = append(want, ledgerRecord{account: "alice", operation: "login", resource: "console", result: "ok", occurredAt: "2026-10-05T00:30:00Z"})
	responseHash[3] = third.Hash
	verifyLedger(t, path, want, responseHash)
}

func TestPostEventRejectsOversizeBody(t *testing.T) {
	// One byte over the limit inside the JSON document.
	t.Run("over by one padding byte", func(t *testing.T) {
		body, _ := exactSizeEventBody(t, `"账号"`, "", "", bodyLimitOver, asciiPad)
		verifyFailedAppendLeavesChainUntouched(t, func(t *testing.T, router http.Handler) *httptest.ResponseRecorder {
			return postEventBody(t, router, body)
		}, "账号")
	})

	// One byte over the limit, and the extra byte is nothing but legal
	// trailing whitespace after an otherwise exactly-maximum document.
	t.Run("over by one trailing whitespace", func(t *testing.T) {
		body, _ := exactSizeEventBody(t, `"账号"`, "", "", bodyLimit, asciiPad)
		verifyFailedAppendLeavesChainUntouched(t, func(t *testing.T, router http.Handler) *httptest.ResponseRecorder {
			return postEventBody(t, router, body+" ")
		}, "账号")
	})
}

func TestPostEventBodyLimitCountsUTF8Bytes(t *testing.T) {
	// A 1 MiB body of mostly 3-byte characters has far fewer than 1 MiB
	// characters; it is accepted, so the limit cannot be counting runes.
	t.Run("multibyte body at the limit is accepted", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "service.db")
		st := openStoreAt(t, path)
		router := NewRouter(st)

		body, resource := exactSizeEventBody(t, `"账号😀"`, "", "", bodyLimit, multibytePad)
		if runes := utf8.RuneCountInString(body); runes >= bodyLimit {
			t.Fatalf("body has %d runes, want far fewer than %d so only the byte count reaches the limit", runes, bodyLimit)
		}
		event := mustPostSizedBody(t, router, body, 1, strings.Repeat("0", 64), "账号😀", resource)

		closeStore(t, st)
		verifyLedger(t, path,
			[]ledgerRecord{{account: "账号😀", operation: "o", resource: resource, result: "ok", occurredAt: sizedBodyOccurredUTC}},
			map[int64]string{1: event.Hash})
	})

	// One byte over the limit the same body is rejected, even though its
	// character count is still far below 1 MiB.
	t.Run("multibyte body one byte over is rejected", func(t *testing.T) {
		body, _ := exactSizeEventBody(t, `"账号😀"`, "", "", bodyLimitOver, multibytePad)
		if runes := utf8.RuneCountInString(body); runes >= bodyLimit {
			t.Fatalf("body has %d runes, want far fewer than %d: a character limit would accept it", runes, bodyLimit)
		}
		verifyFailedAppendLeavesChainUntouched(t, func(t *testing.T, router http.Handler) *httptest.ResponseRecorder {
			return postEventBody(t, router, body)
		}, "账号😀")
	})
}

func TestPostEventBodyReadFailure(t *testing.T) {
	completeObject := `{"account":"ghost-complete","operation":"login","resource":"console","result":"ok","occurred_at":"2026-10-05T00:00:00Z"}`
	completeExistingSeq := `{"account":"ghost-seq","operation":"login","resource":"console","result":"ok","occurred_at":"2026-10-05T00:00:00Z","seq":1}`
	cases := []struct {
		name   string
		prefix string // body bytes delivered before the read fails
		ghost  string // account inside the failing body, must never be saved
	}{
		{"no data before failure", "", ""},
		{"partial object before failure", `{"account":"ghost-partial","operation":"wri`, "ghost-partial"},
		{"complete object before failure", completeObject, "ghost-complete"},
		// seq 1 already exists after the seed: the read failure must still
		// surface as 400 invalid_audit_input, never as 409.
		{"complete object with existing seq before failure", completeExistingSeq, "ghost-seq"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verifyFailedAppendLeavesChainUntouched(t, func(t *testing.T, router http.Handler) *httptest.ResponseRecorder {
				return postEventReader(t, router, &flakyReader{prefix: []byte(tc.prefix)})
			}, tc.ghost)
		})
	}
}

// A complete, valid body read all the way to EOF is still accepted.
func TestPostEventCompleteBodyReadToEOF(t *testing.T) {
	router := NewRouter(openTestStore(t))
	recorder := postEventReader(t, router, strings.NewReader(validEventBody()))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", recorder.Code, recorder.Body.String())
	}
	if event := decodeCreated(t, recorder); event.Seq != 1 {
		t.Fatalf("seq = %d, want 1", event.Seq)
	}
}

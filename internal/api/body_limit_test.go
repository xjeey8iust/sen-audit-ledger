package api

// Regression tests for the POST /events request-body read path:
//
//   - the raw request body is bounded at 1 MiB where JSON structure, JSON
//     escapes and leading/trailing whitespace all count as bytes; 1048575
//     and 1048576 bytes return 201, 1048577 bytes return 400
//     invalid_audit_input even when the extra byte is trailing whitespace;
//   - the bound counts UTF-8 bytes, not characters: the same business text
//     is submitted as direct multibyte text and as ASCII \uXXXX escapes,
//     and a rejected body can contain fewer runes than an accepted one;
//   - a transport-level read error after no bytes, a partial object or a
//     complete (even seq-conflicting) object is the same 400
//     invalid_audit_input, saves nothing and must not turn into 409, while
//     the identical bytes ending in clean EOF still return 201/409;
//   - rejected reads consume no sequence number and move no chain tail:
//     the next legal append takes the next seq with prev_hash equal to the
//     last committed hash, after a health probe, after a close/reopen and
//     after another append;
//   - error bodies carry only the published error object with a string code
//     and non-empty message, never the injected read-error text, SQL, stack
//     frames or file paths.
//
// Every expectation is exercised through the public HTTP entry point and
// cross-checked against rows read through an independent SQLite connection;
// no internal parsing function is called by these tests.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// bodyLimit pins the published 1 MiB contract independently of the
// implementation constant: these tests would fail if the real limit moved.
const (
	bodyLimit      = 1 << 20
	bodyLimitMinus = bodyLimit - 1
	bodyLimitPlus  = bodyLimit + 1
)

// Multibyte sample used two ways at identical logical length: raw UTF-8
// ("账号😀" = 3 runes, 10 bytes per repetition) versus the equivalent ASCII
// JSON escapes (24 bytes per repetition). Both decode to the same text.
const unicodeHeadRepetitions = 200

var (
	// directUnicodeHead is the raw UTF-8 form (10 bytes per repetition).
	directUnicodeHead = strings.Repeat("账号😀", unicodeHeadRepetitions)
	// escapedUnicodeHead is the equivalent all-ASCII JSON escape form:
	// the two Chinese characters as lowercase \uXXXX escapes plus the
	// U+1F600 surrogate pair, 24 bytes per repetition. It decodes to the
	// same three-rune text as directUnicodeHead.
	escapedUnicodeHead = strings.Repeat(`\u8d26\u53f7\uD83D\uDE00`, unicodeHeadRepetitions)
	decodedUnicodeHead = strings.Repeat("账号😀", unicodeHeadRepetitions)
)

// buildSizedEventBody constructs a legal POST /events document of exactly
// size raw bytes. lead/tail are unframed whitespace around the JSON object
// and count toward the length like every other byte. The account value
// begins with head (raw UTF-8 or literal JSON \uXXXX escapes) and is filled
// to length with ASCII 'a' bytes. It returns the body and the number of pad
// bytes used, so callers can predict the decoded account text.
func buildSizedEventBody(t *testing.T, size int, head, lead, tail string) ([]byte, int) {
	t.Helper()
	const marker = "__PAD_MARKER__"
	object := []byte(`{"account":"` + head + marker +
		`","operation":"op","resource":"res","result":"ok","occurred_at":"2026-10-05T00:00:00Z"}`)
	fixed := len(lead) + len(object) - len(marker) + len(tail)
	if fixed > size {
		t.Fatalf("fixed body parts already span %d bytes, cannot build %d-byte body", fixed, size)
	}
	padN := size - fixed
	padded := bytes.Replace(object, []byte(marker), bytes.Repeat([]byte("a"), padN), 1)
	body := make([]byte, 0, size)
	body = append(body, lead...)
	body = append(body, padded...)
	body = append(body, tail...)
	if len(body) != size {
		t.Fatalf("built body is %d bytes, want exactly %d", len(body), size)
	}
	return body, padN
}

// postEventReader sends one POST /events with an arbitrary request body
// stream, allowing read errors to be injected mid-request.
func postEventReader(t *testing.T, router http.Handler, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/events", body))
	return recorder
}

// injectedReadErrorReader serves data once, then fails every subsequent
// Read with errInjectedBodyRead instead of returning io.EOF. The error text
// deliberately contains host, path and source-file tokens so tests can
// prove the public response leaks none of it.
type injectedReadErrorReader struct {
	data []byte
	pos  int
}

// errInjectedBodyRead stands in for an interrupted request-body transfer.
var errInjectedBodyRead = errors.New("read tcp 10.0.0.9:443: injected body read failure at /tmp/fake/conn.go:12")

func (r *injectedReadErrorReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, errInjectedBodyRead
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

// sentinelLeakTokens are substrings of the injected read error that must
// never appear in an HTTP response.
var sentinelLeakTokens = []string{
	"injected body read failure",
	"10.0.0.9",
	"/tmp/fake",
	"conn.go",
}

// withExplicitSeq inserts an explicit seq clause into a legal event body.
func withExplicitSeq(body string, seq int64) string {
	return strings.TrimSuffix(body, "}") + fmt.Sprintf(`,"seq":%d}`, seq)
}

func TestPostEventBodySizeBoundary(t *testing.T) {
	cases := []struct {
		name     string
		size     int
		head     string // raw bytes placed at the start of the account value
		wantHead string // decoded business text head must equal
		lead     string // leading whitespace bytes, counted in the length
		tail     string // trailing whitespace bytes, counted in the length
	}{
		{"1048575 direct multibyte", bodyLimitMinus, directUnicodeHead, decodedUnicodeHead, "", ""},
		{"1048576 direct multibyte", bodyLimit, directUnicodeHead, decodedUnicodeHead, "", ""},
		{"1048577 direct multibyte", bodyLimitPlus, directUnicodeHead, decodedUnicodeHead, "", ""},
		{"1048575 escaped unicode", bodyLimitMinus, escapedUnicodeHead, decodedUnicodeHead, "", ""},
		{"1048576 escaped unicode", bodyLimit, escapedUnicodeHead, decodedUnicodeHead, "", ""},
		{"1048577 escaped unicode", bodyLimitPlus, escapedUnicodeHead, decodedUnicodeHead, "", ""},
		{"1048575 with leading whitespace", bodyLimitMinus, "a", "a", " \n\t\r ", ""},
		{"1048576 with leading whitespace", bodyLimit, "a", "a", " \n\t\r ", ""},
		{"1048577 trailing space only", bodyLimitPlus, "a", "a", "", " "},
		{"1048577 trailing newline only", bodyLimitPlus, "a", "a", "", "\n"},
	}

	statuses := map[string]int{}
	runeCounts := map[string]int{}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, padN := buildSizedEventBody(t, tc.size, tc.head, tc.lead, tc.tail)
			if !utf8.Valid(body) {
				t.Fatalf("test fixture is not valid UTF-8")
			}
			statuses[tc.name] = 0
			runeCounts[tc.name] = utf8.RuneCount(body)

			path := filepath.Join(t.TempDir(), "service.db")
			st := openStoreAt(t, path)
			defer closeStore(t, st)
			router := NewRouter(st)

			recorder := postEventReader(t, router, bytes.NewReader(body))

			if tc.size <= bodyLimit {
				statuses[tc.name] = http.StatusCreated
				if recorder.Code != http.StatusCreated {
					t.Fatalf("status = %d, want 201 for %d-byte body (%s)",
						recorder.Code, tc.size, recorder.Body.String())
				}
				got := decodeCreated(t, recorder)
				if got.Seq != 1 || got.PrevHash != strings.Repeat("0", 64) {
					t.Fatalf("response = %+v, want seq 1 chained to genesis", got)
				}
				wantAccount := tc.wantHead + strings.Repeat("a", padN)
				if got.Account != wantAccount {
					t.Fatalf("account = %q (len %d), want decoded original text %q (len %d)",
						got.Account, len(got.Account), wantAccount, len(wantAccount))
				}
				if got.Operation != "op" || got.Resource != "res" || got.Result != "ok" ||
					got.OccurredAt != "2026-10-05T00:00:00Z" {
					t.Fatalf("business fields = %+v", got)
				}
				// Hash follows the public SHA-256 rule over the decoded text.
				if want := wantHash(1, wantAccount, "op", "res", "ok",
					"2026-10-05T00:00:00Z", strings.Repeat("0", 64)); got.Hash != want {
					t.Fatalf("hash = %s, want %s", got.Hash, want)
				}

				// The 201 response matches the row actually persisted, read
				// back through a connection the service does not own.
				events := readFullLedger(t, path)
				if len(events) != 1 {
					t.Fatalf("persisted %d events, want 1: %+v", len(events), events)
				}
				row := events[0]
				if row.seq != 1 || row.account != wantAccount || row.operation != "op" ||
					row.resource != "res" || row.result != "ok" ||
					row.occurredAt != "2026-10-05T00:00:00Z" ||
					row.prevHash != strings.Repeat("0", 64) || row.hash != got.Hash {
					t.Fatalf("persisted row = %+v, want the decoded 201 record", row)
				}
				return
			}

			statuses[tc.name] = http.StatusBadRequest
			assertErrorEnvelope(t, recorder, http.StatusBadRequest, "invalid_audit_input")
			if events := readFullLedger(t, path); len(events) != 0 {
				t.Fatalf("rejected %d-byte body persisted %d events: %+v", tc.size, len(events), events)
			}

			// When the only overshoot is trailing whitespace, the framed
			// object itself is exactly at the limit and must succeed: one
			// whitespace byte is what flips the answer to 400.
			if tc.tail != "" {
				trimmed := bytes.TrimRight(body, " \t\r\n")
				if len(trimmed) != bodyLimit {
					t.Fatalf("trimmed body = %d bytes, want %d", len(trimmed), bodyLimit)
				}
				ok := postEventReader(t, router, bytes.NewReader(trimmed))
				if ok.Code != http.StatusCreated {
					t.Fatalf("same object without trailing whitespace: status = %d, want 201 (%s)",
						ok.Code, ok.Body.String())
				}
				if events := readFullLedger(t, path); len(events) != 1 {
					t.Fatalf("persisted %d events after trimmed append, want 1", len(events))
				}
			}
		})
	}

	// Byte-vs-character proof: the accepted all-ASCII escape document
	// contains MORE runes than the rejected direct-multibyte document. A
	// character-counted limit could not produce this pairing.
	const rejected = "1048577 direct multibyte"
	const accepted = "1048576 escaped unicode"
	if statuses[accepted] != http.StatusCreated {
		t.Fatalf("%s was not accepted", accepted)
	}
	if statuses[rejected] != http.StatusBadRequest {
		t.Fatalf("%s was not rejected", rejected)
	}
	if runeCounts[rejected] >= runeCounts[accepted] {
		t.Fatalf("limit seems character-based: rejected %q has %d runes, accepted %q has %d runes",
			rejected, runeCounts[rejected], accepted, runeCounts[accepted])
	}
}

func TestPostEventReadFailuresBehaveLikeInvalidInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st := openStoreAt(t, path)
	defer closeStore(t, st)
	router := NewRouter(st)

	// One committed record so seq 1 exists and seq 9 is a gap.
	seed := decodeCreated(t, postEventBody(t, router, validEventBody()))
	if seed.Seq != 1 {
		t.Fatalf("seed seq = %d, want 1", seed.Seq)
	}

	complete := validEventBody()
	completeSeq1 := withExplicitSeq(complete, 1)
	completeSeq9 := withExplicitSeq(complete, 9)

	cases := []struct {
		name string
		data []byte
	}{
		{"no data before read error", nil},
		{"partial object then read error", []byte(`{"account":"alice","operation":"login",`)},
		{"complete object then read error", []byte(complete)},
		{"complete existing-seq object then read error", []byte(completeSeq1)},
		{"complete gap-seq object then read error", []byte(completeSeq9)},
	}

	uniformMessage := ""
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := postEventReader(t, router, &injectedReadErrorReader{data: tc.data})
			assertErrorEnvelope(t, recorder, http.StatusBadRequest, "invalid_audit_input")

			errObj := decodeEvent(t, recorder)["error"].(map[string]any)
			message := errObj["message"].(string)
			if uniformMessage == "" {
				uniformMessage = message
			} else if message != uniformMessage {
				t.Fatalf("message = %q, want the same 400 message %q for every read failure",
					message, uniformMessage)
			}
			for _, leak := range sentinelLeakTokens {
				if strings.Contains(recorder.Body.String(), leak) {
					t.Fatalf("response leaks read-error detail %q: %s", leak, recorder.Body.String())
				}
			}

			// Whatever bytes arrived, nothing was saved.
			events := readFullLedger(t, path)
			if len(events) != 1 || events[0].hash != seed.Hash {
				t.Fatalf("ledger = %+v after read failure, want only the seed record", events)
			}
		})
	}

	// Storage stayed usable throughout the body-read failures.
	if recorder := getHealthz(t, router); recorder.Code != http.StatusOK {
		t.Fatalf("healthz during read failures: status = %d (%s)",
			recorder.Code, recorder.Body.String())
	} else if recorder.Body.String() != `{"database":"ok","status":"ok"}` {
		t.Fatalf("healthz body = %s", recorder.Body.String())
	}

	// Controls with the IDENTICAL bytes ending in clean EOF: a complete
	// legal object is appended (201), the existing seq conflicts (409) and
	// the gap seq is invalid input (400). The read error is what forced all
	// three shapes to 400 above; an already-complete object must not be
	// saved, and an existing seq must not surface as 409.
	recorder := postEventBody(t, router, complete)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("complete object with clean EOF: status = %d, want 201 (%s)",
			recorder.Code, recorder.Body.String())
	}
	followup := decodeCreated(t, recorder)
	if followup.Seq != 2 || followup.PrevHash != seed.Hash {
		t.Fatalf("clean append = %+v, want seq 2 chained to seed %s", followup, seed.Hash)
	}

	recorder = postEventBody(t, router, completeSeq1)
	assertErrorEnvelope(t, recorder, http.StatusConflict, "audit_seq_conflict")

	recorder = postEventBody(t, router, completeSeq9)
	assertErrorEnvelope(t, recorder, http.StatusBadRequest, "invalid_audit_input")
}

func TestPostEventRejectedBodyReadsLeaveChainAndSequenceUntouched(t *testing.T) {
	complete := validEventBody()
	completeSeq1 := withExplicitSeq(complete, 1)
	completeSeq9 := withExplicitSeq(complete, 9)

	scenarios := []struct {
		name       string
		newReader  func() io.Reader
		leakTokens []string
	}{
		{"oversize by one byte", func() io.Reader {
			body, _ := buildSizedEventBody(t, bodyLimitPlus, directUnicodeHead, "", "")
			return bytes.NewReader(body)
		}, nil},
		{"oversize by trailing space", func() io.Reader {
			body, _ := buildSizedEventBody(t, bodyLimitPlus, "a", "", " ")
			return bytes.NewReader(body)
		}, nil},
		{"oversize by trailing newline", func() io.Reader {
			body, _ := buildSizedEventBody(t, bodyLimitPlus, "a", "", "\n")
			return bytes.NewReader(body)
		}, nil},
		{"read error before any byte", func() io.Reader {
			return &injectedReadErrorReader{}
		}, sentinelLeakTokens},
		{"read error with partial object", func() io.Reader {
			return &injectedReadErrorReader{data: []byte(`{"account":"alice","operation":"login",`)}
		}, sentinelLeakTokens},
		{"read error after complete object", func() io.Reader {
			return &injectedReadErrorReader{data: []byte(complete)}
		}, sentinelLeakTokens},
		{"read error after complete object with existing seq", func() io.Reader {
			return &injectedReadErrorReader{data: []byte(completeSeq1)}
		}, sentinelLeakTokens},
		{"read error after complete object with gap seq", func() io.Reader {
			return &injectedReadErrorReader{data: []byte(completeSeq9)}
		}, sentinelLeakTokens},
	}

	seedRecord := ledgerRecord{
		account: "alice", operation: "login", resource: "console", result: "ok",
		occurredAt: "2026-10-05T00:30:00Z",
	}
	uniformMessage := ""

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "service.db")
			st := openStoreAt(t, path)
			router := NewRouter(st)

			// 1. One committed record anchors the chain.
			seed := decodeCreated(t, postEventBody(t, router, validEventBody()))
			if seed.Seq != 1 || seed.PrevHash != strings.Repeat("0", 64) {
				t.Fatalf("seed = %+v, want seq 1 on genesis", seed)
			}

			// 2. The oversize or failed read returns the published 400 shape.
			recorder := postEventReader(t, router, sc.newReader())
			assertErrorEnvelope(t, recorder, http.StatusBadRequest, "invalid_audit_input")
			message := decodeEvent(t, recorder)["error"].(map[string]any)["message"].(string)
			if uniformMessage == "" {
				uniformMessage = message
			} else if message != uniformMessage {
				t.Fatalf("message = %q, want the same 400 message %q", message, uniformMessage)
			}
			for _, leak := range sc.leakTokens {
				if strings.Contains(recorder.Body.String(), leak) {
					t.Fatalf("response leaks read-error detail %q: %s", leak, recorder.Body.String())
				}
			}

			// 3. Storage is available and the chain is unchanged.
			if healthz := getHealthz(t, router); healthz.Code != http.StatusOK ||
				healthz.Body.String() != `{"database":"ok","status":"ok"}` {
				t.Fatalf("healthz = %d %s, want 200 ok", healthz.Code, healthz.Body.String())
			}
			if events := readFullLedger(t, path); len(events) != 1 || events[0].hash != seed.Hash {
				t.Fatalf("ledger after failure = %+v, want only seed %s", events, seed.Hash)
			}

			// 4. The failed request consumed no seq: the next append is seq
			// 2 and chains directly onto the previously committed record.
			followup := decodeCreated(t, postEventBody(t, router, validEventBody()))
			if followup.Seq != 2 || followup.PrevHash != seed.Hash {
				t.Fatalf("followup = %+v, want seq 2 with prev_hash %s", followup, seed.Hash)
			}

			want := []ledgerRecord{seedRecord, seedRecord}
			closeStore(t, st)
			verifyLedger(t, path, want, map[int64]string{
				1: seed.Hash,
				2: followup.Hash,
			})

			// 5. After close/reopen records and chain tail are identical and
			// the next append continues the same sequence.
			st = openStoreAt(t, path)
			afterReopen := decodeCreated(t, postEventBody(t, NewRouter(st), validEventBody()))
			if afterReopen.Seq != 3 || afterReopen.PrevHash != followup.Hash {
				t.Fatalf("after reopen = %+v, want seq 3 chained to %s", afterReopen, followup.Hash)
			}
			closeStore(t, st)

			verifyLedger(t, path, []ledgerRecord{seedRecord, seedRecord, seedRecord},
				map[int64]string{1: seed.Hash, 2: followup.Hash, 3: afterReopen.Hash})
		})
	}
}

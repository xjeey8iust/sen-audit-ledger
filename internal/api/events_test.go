package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/xjeey8iust/sen-audit-ledger/internal/store"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "service.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func postEventBody(t *testing.T, router http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(body))
	router.ServeHTTP(recorder, request)
	return recorder
}

func validEventBody() string {
	return `{"account":"alice","operation":"login","resource":"console","result":"ok","occurred_at":"2026-10-05T08:30:00+08:00"}`
}

func decodeEvent(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return decoded
}

func wantHash(seq int64, account, operation, resource, result, occurredAt, prevHash string) string {
	payload := fmt.Sprintf(`[%d,"%s","%s","%s","%s","%s","%s"]`, seq, account, operation, resource, result, occurredAt, prevHash)
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

func TestPostEventAppendsGenesisRecord(t *testing.T) {
	router := NewRouter(openTestStore(t))

	recorder := postEventBody(t, router, validEventBody())
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d (%s)", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
	event := decodeEvent(t, recorder)

	if event["seq"] != float64(1) {
		t.Fatalf("seq = %v, want 1", event["seq"])
	}
	if event["prev_hash"] != strings.Repeat("0", 64) {
		t.Fatalf("prev_hash = %v, want 64 zeros", event["prev_hash"])
	}
	// occurred_at is stored and returned in UTC, fraction preserved.
	if event["occurred_at"] != "2026-10-05T00:30:00Z" {
		t.Fatalf("occurred_at = %v", event["occurred_at"])
	}
	want := wantHash(1, "alice", "login", "console", "ok", "2026-10-05T00:30:00Z", strings.Repeat("0", 64))
	if event["hash"] != want {
		t.Fatalf("hash = %v, want %v", event["hash"], want)
	}
}

func TestPostEventChainsRecords(t *testing.T) {
	router := NewRouter(openTestStore(t))

	first := decodeEvent(t, postEventBody(t, router, validEventBody()))
	second := decodeEvent(t, postEventBody(t, router,
		`{"account":"bob","operation":"read","resource":"report","result":"denied","occurred_at":"2026-10-05T01:00:00.250Z"}`))

	if second["seq"] != float64(2) {
		t.Fatalf("seq = %v, want 2", second["seq"])
	}
	if second["prev_hash"] != first["hash"] {
		t.Fatalf("prev_hash = %v, want previous hash %v", second["prev_hash"], first["hash"])
	}
	if second["occurred_at"] != "2026-10-05T01:00:00.250Z" {
		t.Fatalf("occurred_at = %v, fraction not preserved", second["occurred_at"])
	}
}

func TestPostEventKeepsOriginalFieldText(t *testing.T) {
	router := NewRouter(openTestStore(t))

	recorder := postEventBody(t, router,
		`{"account":"  alice  ","operation":"login","resource":"console","result":"ok","occurred_at":"2026-10-05T00:00:00Z"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if event := decodeEvent(t, recorder); event["account"] != "  alice  " {
		t.Fatalf("account = %q, want original text kept", event["account"])
	}
}

func TestPostEventRejectsInvalidInput(t *testing.T) {
	cases := map[string]string{
		"malformed json":      `{"account":`,
		"trailing value":      `{} {}`,
		"non object array":    `[]`,
		"non object scalar":   `42`,
		"null document":       `null`,
		"missing field":       `{"account":"a","operation":"o","resource":"r","occurred_at":"2026-10-05T00:00:00Z"}`,
		"null field":          `{"account":null,"operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z"}`,
		"wrong type":          `{"account":1,"operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z"}`,
		"blank account":       `{"account":"   ","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z"}`,
		"forbidden hash":      `{"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z","hash":"x"}`,
		"forbidden prev_hash": `{"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z","prev_hash":"x"}`,
		"unknown field":       `{"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z","note":"x"}`,
		"time no zone":        `{"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00"}`,
		"time bad date":       `{"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":"2026-02-30T00:00:00Z"}`,
		"time leap second":    `{"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":"2016-12-31T23:59:60Z"}`,
		"time bad shape":      `{"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05 00:00:00Z"}`,
		"seq fraction":        `{"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z","seq":1.5}`,
		"seq exponent":        `{"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z","seq":1e3}`,
		"seq string":          `{"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z","seq":"1"}`,
		"seq zero":            `{"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z","seq":0}`,
		"seq negative":        `{"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z","seq":-1}`,
		"seq null":            `{"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z","seq":null}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			router := NewRouter(openTestStore(t))
			recorder := postEventBody(t, router, body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d (%s)", recorder.Code, http.StatusBadRequest, recorder.Body.String())
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
			if decoded.Error.Code != "invalid_audit_input" {
				t.Fatalf("code = %q", decoded.Error.Code)
			}
			if decoded.Error.Message == "" {
				t.Fatal("message must be non-empty")
			}
		})
	}
}

func TestPostEventExplicitSeq(t *testing.T) {
	router := NewRouter(openTestStore(t))

	// Explicit seq equal to the next sequence number is accepted.
	body := `{"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z","seq":1}`
	if recorder := postEventBody(t, router, body); recorder.Code != http.StatusCreated {
		t.Fatalf("explicit next seq: status = %d (%s)", recorder.Code, recorder.Body.String())
	}

	// An existing seq conflicts.
	if recorder := postEventBody(t, router, body); recorder.Code != http.StatusConflict {
		t.Fatalf("existing seq: status = %d, want 409 (%s)", recorder.Code, recorder.Body.String())
	} else if event := decodeEvent(t, recorder); event["error"].(map[string]any)["code"] != "audit_seq_conflict" {
		t.Fatalf("existing seq: code = %v", event)
	}

	// A seq past the next one is rejected as invalid input.
	gap := `{"account":"a","operation":"o","resource":"r","result":"ok","occurred_at":"2026-10-05T00:00:00Z","seq":5}`
	recorder := postEventBody(t, router, gap)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("gap seq: status = %d, want 400 (%s)", recorder.Code, recorder.Body.String())
	}
	if event := decodeEvent(t, recorder); event["error"].(map[string]any)["code"] != "invalid_audit_input" {
		t.Fatalf("gap seq: body = %v", event)
	}

	// Failed appends consume no sequence number: the next append is seq 2.
	recorder = postEventBody(t, router, validEventBody())
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if event := decodeEvent(t, recorder); event["seq"] != float64(2) {
		t.Fatalf("seq = %v, want 2 after failed appends", event["seq"])
	}
}

func TestPostEventContinuesAfterReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")

	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	first := decodeEvent(t, postEventBody(t, NewRouter(st), validEventBody()))
	st.Close()

	st, err = store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st.Close()
	second := decodeEvent(t, postEventBody(t, NewRouter(st), validEventBody()))

	if second["seq"] != float64(2) || second["prev_hash"] != first["hash"] {
		t.Fatalf("after reopen: seq = %v prev_hash = %v, want 2 and %v", second["seq"], second["prev_hash"], first["hash"])
	}
}

func TestPostEventConcurrentAppendsStayGapFree(t *testing.T) {
	router := NewRouter(openTestStore(t))

	const writers = 16
	codes := make([]int, writers)
	seqs := make([]float64, writers)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			recorder := postEventBody(t, router, validEventBody())
			codes[i] = recorder.Code
			if recorder.Code == http.StatusCreated {
				seqs[i] = decodeEvent(t, recorder)["seq"].(float64)
			}
		}(i)
	}
	wg.Wait()

	seen := map[float64]bool{}
	for i := 0; i < writers; i++ {
		if codes[i] != http.StatusCreated {
			t.Fatalf("writer %d: status = %d, want 201", i, codes[i])
		}
		if seen[seqs[i]] {
			t.Fatalf("duplicate seq %v", seqs[i])
		}
		seen[seqs[i]] = true
	}
	for seq := float64(1); seq <= writers; seq++ {
		if !seen[seq] {
			t.Fatalf("seq %v missing, got %v", seq, seen)
		}
	}
}

func TestPostEventStorageFailureReturns503(t *testing.T) {
	st := openTestStore(t)
	router := NewRouter(st)
	st.Close()

	recorder := postEventBody(t, router, validEventBody())
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (%s)", recorder.Code, recorder.Body.String())
	}
	if event := decodeEvent(t, recorder); event["error"].(map[string]any)["code"] != "storage_unavailable" {
		t.Fatalf("body = %v", event)
	}
}

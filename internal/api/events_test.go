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

func newTestRouter(t *testing.T) (*store.Store, http.Handler) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "service.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st, NewRouter(st)
}

func postEvents(t *testing.T, handler http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(body))
	handler.ServeHTTP(recorder, request)
	return recorder
}

func decodeBody(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("response is not JSON: %v: %q", err, recorder.Body.String())
	}
	return decoded
}

func wantError(t *testing.T, recorder *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status = %d, want %d (body %s)", recorder.Code, status, recorder.Body.String())
	}
	decoded := decodeBody(t, recorder)
	errObj, ok := decoded["error"].(map[string]any)
	if !ok {
		t.Fatalf("no top-level error object in %v", decoded)
	}
	if errObj["code"] != code {
		t.Fatalf("error code = %v, want %q", errObj["code"], code)
	}
	message, ok := errObj["message"].(string)
	if !ok || message == "" {
		t.Fatalf("error message must be a non-empty string, got %v", errObj["message"])
	}
	for _, banned := range []string{"sql", "goroutine", ".go:", "/"} {
		if strings.Contains(strings.ToLower(message), banned) {
			t.Fatalf("error message %q leaks internal detail %q", message, banned)
		}
	}
}

const validBody = `{"account":"alice","operation":"login","resource":"console","result":"success","occurred_at":"2026-10-05T08:30:00.250+08:00"}`

func TestCreateEventReturnsPersistedRecord(t *testing.T) {
	_, handler := newTestRouter(t)

	recorder := postEvents(t, handler, validBody)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d (body %s)", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
	record := decodeBody(t, recorder)

	if record["seq"] != float64(1) {
		t.Fatalf("seq = %v, want 1", record["seq"])
	}
	if record["account"] != "alice" || record["operation"] != "login" || record["resource"] != "console" || record["result"] != "success" {
		t.Fatalf("business fields wrong: %v", record)
	}
	// Offset converted to UTC, fractional seconds preserved.
	if record["occurred_at"] != "2026-10-05T00:30:00.250Z" {
		t.Fatalf("occurred_at = %v, want 2026-10-05T00:30:00.250Z", record["occurred_at"])
	}
	zeroes := strings.Repeat("0", 64)
	if record["prev_hash"] != zeroes {
		t.Fatalf("prev_hash = %v, want 64 zeros", record["prev_hash"])
	}
	payload := fmt.Sprintf(`[1,"alice","login","console","success","2026-10-05T00:30:00.250Z","%s"]`, zeroes)
	sum := sha256.Sum256([]byte(payload))
	if record["hash"] != hex.EncodeToString(sum[:]) {
		t.Fatalf("hash = %v, want %x", record["hash"], sum)
	}

	second := postEvents(t, handler, `{"account":"bob","operation":"read","resource":"report","result":"ok","occurred_at":"2026-10-05T00:31:00Z"}`)
	if second.Code != http.StatusCreated {
		t.Fatalf("second status = %d (body %s)", second.Code, second.Body.String())
	}
	record2 := decodeBody(t, second)
	if record2["seq"] != float64(2) || record2["prev_hash"] != record["hash"] {
		t.Fatalf("chain broken: second = %v, first hash = %v", record2, record["hash"])
	}
}

func TestCreateEventKeepsOriginalWhitespace(t *testing.T) {
	_, handler := newTestRouter(t)
	recorder := postEvents(t, handler, `{"account":"  alice  ","operation":"op","resource":"res","result":"ok","occurred_at":"2026-10-05T00:00:00Z"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	if record := decodeBody(t, recorder); record["account"] != "  alice  " {
		t.Fatalf("account = %v, want original whitespace kept", record["account"])
	}
}

func TestCreateEventRejectsInvalidInput(t *testing.T) {
	cases := map[string]string{
		"malformed json":       `{"account":`,
		"trailing garbage":     `{"account":"a"} junk`,
		"two values":           `{}{}`,
		"non object array":     `[1,2]`,
		"non object string":    `"hello"`,
		"missing account":      `{"operation":"op","resource":"res","result":"ok","occurred_at":"2026-10-05T00:00:00Z"}`,
		"null account":         `{"account":null,"operation":"op","resource":"res","result":"ok","occurred_at":"2026-10-05T00:00:00Z"}`,
		"numeric account":      `{"account":7,"operation":"op","resource":"res","result":"ok","occurred_at":"2026-10-05T00:00:00Z"}`,
		"blank account":        `{"account":"   ","operation":"op","resource":"res","result":"ok","occurred_at":"2026-10-05T00:00:00Z"}`,
		"empty operation":      `{"account":"a","operation":"","resource":"res","result":"ok","occurred_at":"2026-10-05T00:00:00Z"}`,
		"missing occurred_at":  `{"account":"a","operation":"op","resource":"res","result":"ok"}`,
		"occurred_at no zone":  `{"account":"a","operation":"op","resource":"res","result":"ok","occurred_at":"2026-10-05T00:00:00"}`,
		"occurred_at bad date": `{"account":"a","operation":"op","resource":"res","result":"ok","occurred_at":"2026-13-05T00:00:00Z"}`,
		"occurred_at leap sec": `{"account":"a","operation":"op","resource":"res","result":"ok","occurred_at":"2016-12-31T23:59:60Z"}`,
		"occurred_at not rfc":  `{"account":"a","operation":"op","resource":"res","result":"ok","occurred_at":"yesterday"}`,
		"forbidden hash":       `{"account":"a","operation":"op","resource":"res","result":"ok","occurred_at":"2026-10-05T00:00:00Z","hash":"x"}`,
		"forbidden prev_hash":  `{"account":"a","operation":"op","resource":"res","result":"ok","occurred_at":"2026-10-05T00:00:00Z","prev_hash":"x"}`,
		"unknown field":        `{"account":"a","operation":"op","resource":"res","result":"ok","occurred_at":"2026-10-05T00:00:00Z","extra":1}`,
		"seq decimal":          `{"account":"a","operation":"op","resource":"res","result":"ok","occurred_at":"2026-10-05T00:00:00Z","seq":1.5}`,
		"seq integral float":   `{"account":"a","operation":"op","resource":"res","result":"ok","occurred_at":"2026-10-05T00:00:00Z","seq":1.0}`,
		"seq exponent":         `{"account":"a","operation":"op","resource":"res","result":"ok","occurred_at":"2026-10-05T00:00:00Z","seq":1e1}`,
		"seq negative":         `{"account":"a","operation":"op","resource":"res","result":"ok","occurred_at":"2026-10-05T00:00:00Z","seq":-1}`,
		"seq zero":             `{"account":"a","operation":"op","resource":"res","result":"ok","occurred_at":"2026-10-05T00:00:00Z","seq":0}`,
		"seq string":           `{"account":"a","operation":"op","resource":"res","result":"ok","occurred_at":"2026-10-05T00:00:00Z","seq":"1"}`,
		"seq gap":              `{"account":"a","operation":"op","resource":"res","result":"ok","occurred_at":"2026-10-05T00:00:00Z","seq":5}`,
		"empty body":           ``,
		"whitespace only body": `   `,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, handler := newTestRouter(t)
			recorder := postEvents(t, handler, body)
			wantError(t, recorder, http.StatusBadRequest, "invalid_audit_input")
		})
	}
}

func TestCreateEventSeqNullTreatedAsOmitted(t *testing.T) {
	_, handler := newTestRouter(t)
	recorder := postEvents(t, handler, `{"account":"a","operation":"op","resource":"res","result":"ok","occurred_at":"2026-10-05T00:00:00Z","seq":null}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
}

func TestCreateEventExplicitSeq(t *testing.T) {
	_, handler := newTestRouter(t)

	first := postEvents(t, handler, `{"account":"a","operation":"op","resource":"res","result":"ok","occurred_at":"2026-10-05T00:00:00Z","seq":1}`)
	if first.Code != http.StatusCreated {
		t.Fatalf("explicit seq 1 status = %d (body %s)", first.Code, first.Body.String())
	}

	conflict := postEvents(t, handler, `{"account":"b","operation":"op","resource":"res","result":"ok","occurred_at":"2026-10-05T00:00:00Z","seq":1}`)
	wantError(t, conflict, http.StatusConflict, "audit_seq_conflict")

	gap := postEvents(t, handler, `{"account":"b","operation":"op","resource":"res","result":"ok","occurred_at":"2026-10-05T00:00:00Z","seq":3}`)
	wantError(t, gap, http.StatusBadRequest, "invalid_audit_input")

	next := postEvents(t, handler, `{"account":"b","operation":"op","resource":"res","result":"ok","occurred_at":"2026-10-05T00:00:00Z","seq":2}`)
	if next.Code != http.StatusCreated {
		t.Fatalf("explicit seq 2 status = %d (body %s)", next.Code, next.Body.String())
	}
	if record := decodeBody(t, next); record["seq"] != float64(2) {
		t.Fatalf("seq = %v, want 2", record["seq"])
	}
}

func TestCreateEventConcurrentAppendsAllSucceed(t *testing.T) {
	_, handler := newTestRouter(t)

	const writers = 16
	recorders := make([]*httptest.ResponseRecorder, writers)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"account":"user-%02d","operation":"op","resource":"res","result":"ok","occurred_at":"2026-10-05T00:00:00Z"}`, i)
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(body))
			handler.ServeHTTP(recorder, request)
			recorders[i] = recorder
		}(i)
	}
	wg.Wait()

	seen := map[float64]map[string]any{}
	for i, recorder := range recorders {
		if recorder.Code != http.StatusCreated {
			t.Fatalf("request %d status = %d (body %s)", i, recorder.Code, recorder.Body.String())
		}
		record := decodeBody(t, recorder)
		seq := record["seq"].(float64)
		if _, dup := seen[seq]; dup {
			t.Fatalf("seq %v assigned twice", seq)
		}
		seen[seq] = record
	}
	prevHash := strings.Repeat("0", 64)
	for seq := 1; seq <= writers; seq++ {
		record, ok := seen[float64(seq)]
		if !ok {
			t.Fatalf("seq %d missing", seq)
		}
		if record["prev_hash"] != prevHash {
			t.Fatalf("seq %d prev_hash = %v, want %v", seq, record["prev_hash"], prevHash)
		}
		prevHash = record["hash"].(string)
	}
}

func TestCreateEventStorageUnavailable(t *testing.T) {
	st, handler := newTestRouter(t)
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	recorder := postEvents(t, handler, validBody)
	wantError(t, recorder, http.StatusServiceUnavailable, "storage_unavailable")
}

func TestGetEventsIsNotMatched(t *testing.T) {
	_, handler := newTestRouter(t)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/events", nil)
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNotFound)
	}
}

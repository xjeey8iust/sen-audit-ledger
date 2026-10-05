package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/xjeey8iust/sen-audit-ledger/internal/store"
)

// maxEventBodyBytes bounds one audit event payload.
const maxEventBodyBytes = 1 << 20

// eventResponse is the 201 body for a persisted record.
type eventResponse struct {
	Account    string `json:"account"`
	Operation  string `json:"operation"`
	Resource   string `json:"resource"`
	Result     string `json:"result"`
	OccurredAt string `json:"occurred_at"`
	Seq        int64  `json:"seq"`
	PrevHash   string `json:"prev_hash"`
	Hash       string `json:"hash"`
}

// postEvent appends one audit record to the ledger.
func postEvent(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		input, ok := parseEventInput(c)
		if !ok {
			return
		}
		event, err := st.Append(input)
		if err != nil {
			switch {
			case errors.Is(err, store.ErrSeqConflict):
				writeError(c, http.StatusConflict, "audit_seq_conflict", "seq already exists in the ledger")
			case errors.Is(err, store.ErrSeqGap):
				writeError(c, http.StatusBadRequest, "invalid_audit_input", "seq must be the next sequence number")
			default:
				writeError(c, http.StatusServiceUnavailable, "storage_unavailable", "storage is not available")
			}
			return
		}
		c.JSON(http.StatusCreated, eventResponse{
			Account:    event.Account,
			Operation:  event.Operation,
			Resource:   event.Resource,
			Result:     event.Result,
			OccurredAt: event.OccurredAt,
			Seq:        event.Seq,
			PrevHash:   event.PrevHash,
			Hash:       event.Hash,
		})
	}
}

// parseEventInput validates the request body against the audit contract. On
// any violation it writes the invalid_audit_input error and reports false.
func parseEventInput(c *gin.Context) (store.EventInput, bool) {
	var input store.EventInput
	invalid := func() (store.EventInput, bool) {
		writeError(c, http.StatusBadRequest, "invalid_audit_input", "request body is not a valid audit event")
		return store.EventInput{}, false
	}

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxEventBodyBytes+1))
	if err != nil || len(body) > maxEventBodyBytes {
		return invalid()
	}

	// The body must be losslessly interpretable as Unicode before any JSON
	// decoding runs: encoding/json would otherwise replace malformed UTF-8
	// bytes and unpaired \u surrogate escapes with U+FFFD and let them through
	// as normal audit content. This gate runs before seq checks and storage.
	if !utf8.Valid(body) || !hasValidUnicodeEscapes(body) {
		return invalid()
	}

	var raw map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&raw); err != nil || raw == nil {
		return invalid()
	}
	var extra struct{}
	if err := decoder.Decode(&extra); err != io.EOF {
		return invalid()
	}

	for key := range raw {
		switch key {
		case "account", "operation", "resource", "result", "occurred_at", "seq":
		default:
			// hash, prev_hash and any unknown field are forbidden.
			return invalid()
		}
	}

	var ok bool
	if input.Account, ok = requiredString(raw, "account"); !ok || strings.TrimSpace(input.Account) == "" {
		return invalid()
	}
	if input.Operation, ok = requiredString(raw, "operation"); !ok || strings.TrimSpace(input.Operation) == "" {
		return invalid()
	}
	if input.Resource, ok = requiredString(raw, "resource"); !ok || strings.TrimSpace(input.Resource) == "" {
		return invalid()
	}
	if input.Result, ok = requiredString(raw, "result"); !ok || strings.TrimSpace(input.Result) == "" {
		return invalid()
	}
	occurredAt, ok := requiredString(raw, "occurred_at")
	if !ok {
		return invalid()
	}
	if input.OccurredAt, ok = normalizeOccurredAt(occurredAt); !ok {
		return invalid()
	}

	if rawSeq, present := raw["seq"]; present {
		literal := string(rawSeq)
		if !isUintLiteral(literal) {
			return invalid()
		}
		seq, err := strconv.ParseInt(literal, 10, 64)
		if err != nil || seq <= 0 {
			return invalid()
		}
		input.Seq = &seq
	}
	return input, true
}

// hasValidUnicodeEscapes scans the raw bytes of a JSON document and reports
// every \uXXXX escape inside JSON strings (field names included) to be
// well-formed. A surrogate must be a complete pair with a high surrogate
// (U+D800..U+DBFF) immediately followed by a low surrogate (U+DC00..U+DFFF);
// a lone surrogate or a reversed pair is rejected. A backslash escaped as
// "\\" starts no escape, so the literal text \\uD83D is never mistaken for a
// Unicode escape, and pairing never crosses a string boundary. Malformed
// escapes (\u short of four hex digits) are rejected here as well. The caller
// is expected to have validated the document as UTF-8 and to run a strict JSON
// decode afterwards; this routine only catches what that decode would repair.
func hasValidUnicodeEscapes(body []byte) bool {
	inString := false
	// pendingHigh is set while a string's last \u escape was a high surrogate
	// that the next escape must complete with a low surrogate.
	pendingHigh := false
	for i := 0; i < len(body); i++ {
		b := body[i]
		if !inString {
			if b == '"' {
				inString = true
				pendingHigh = false
			}
			continue
		}
		switch b {
		case '\\':
			if i+1 >= len(body) {
				return false
			}
			esc := body[i+1]
			if esc != 'u' {
				// A doubled backslash consumes both bytes and starts no
				// escape, leaving any following "\uD83D" as literal text.
				// It also cannot complete a pending surrogate pair.
				if pendingHigh {
					return false
				}
				i++
				continue
			}
			r, ok := parseHex4(body, i+2)
			if !ok {
				return false
			}
			i += 5 // backslash, 'u' and four hex digits
			switch {
			case r >= 0xD800 && r <= 0xDBFF:
				if pendingHigh {
					// A high surrogate must be completed by a low one.
					return false
				}
				pendingHigh = true
			case r >= 0xDC00 && r <= 0xDFFF:
				if !pendingHigh {
					// A low surrogate with no preceding high surrogate.
					return false
				}
				pendingHigh = false
			default:
				if pendingHigh {
					// The high surrogate was paired with a non-surrogate.
					return false
				}
			}
		case '"':
			if pendingHigh {
				// The string ends while a high surrogate is unpaired.
				return false
			}
			inString = false
		default:
			// A literal character (or an unescaped control byte, which the
			// JSON decoder rejects separately) cannot complete a pair, so a
			// pending high surrogate here is left unpaired.
			if pendingHigh {
				return false
			}
		}
	}
	return !inString && !pendingHigh
}

// parseHex4 reads four hexadecimal digits at body[pos:] and returns their
// value. It returns false when fewer than four digits remain or any digit is
// not hexadecimal.
func parseHex4(body []byte, pos int) (rune, bool) {
	if pos+4 > len(body) {
		return 0, false
	}
	v := 0
	for k := 0; k < 4; k++ {
		c := body[pos+k]
		switch {
		case c >= '0' && c <= '9':
			c -= '0'
		case c >= 'a' && c <= 'f':
			c -= 'a' - 10
		case c >= 'A' && c <= 'F':
			c -= 'A' - 10
		default:
			return 0, false
		}
		v = v<<4 | int(c)
	}
	return rune(v), true
}

// requiredString extracts a string field that must be present, non-null and
// actually a JSON string.
func requiredString(raw map[string]json.RawMessage, name string) (string, bool) {
	value, present := raw[name]
	if !present || string(value) == "null" {
		return "", false
	}
	var s string
	if err := json.Unmarshal(value, &s); err != nil {
		return "", false
	}
	return s, true
}

// isUintLiteral reports whether the raw JSON value is a non-negative integer
// literal with no fraction, exponent or sign.
func isUintLiteral(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// normalizeOccurredAt parses a strict RFC3339 timestamp with an explicit
// timezone, rejects leap seconds, and returns the UTC form keeping the
// fractional-second digits exactly as supplied.
func normalizeOccurredAt(value string) (string, bool) {
	const headLen = len("2006-01-02T15:04:05")
	if len(value) < headLen+1 {
		return "", false
	}
	head := value[:headLen]
	if head[4] != '-' || head[7] != '-' || head[10] != 'T' || head[13] != ':' || head[16] != ':' {
		return "", false
	}
	field := func(start, width int) (int, bool) {
		v := 0
		for i := start; i < start+width; i++ {
			if head[i] < '0' || head[i] > '9' {
				return 0, false
			}
			v = v*10 + int(head[i]-'0')
		}
		return v, true
	}
	year, ok1 := field(0, 4)
	month, ok2 := field(5, 2)
	day, ok3 := field(8, 2)
	hour, ok4 := field(11, 2)
	minute, ok5 := field(14, 2)
	second, ok6 := field(17, 2)
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || !ok6 {
		return "", false
	}
	if second > 59 {
		// Leap seconds are not accepted.
		return "", false
	}

	rest := value[headLen:]
	fraction := ""
	if strings.HasPrefix(rest, ".") {
		i := 1
		for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
			i++
		}
		if i == 1 {
			return "", false
		}
		fraction = rest[:i]
		rest = rest[i:]
	}

	offsetMinutes := 0
	switch {
	case rest == "Z":
	case len(rest) == 6 && (rest[0] == '+' || rest[0] == '-') && rest[3] == ':' &&
		isDigit(rest[1]) && isDigit(rest[2]) && isDigit(rest[4]) && isDigit(rest[5]):
		oh := int(rest[1]-'0')*10 + int(rest[2]-'0')
		om := int(rest[4]-'0')*10 + int(rest[5]-'0')
		if oh > 23 || om > 59 {
			return "", false
		}
		offsetMinutes = oh*60 + om
		if rest[0] == '-' {
			offsetMinutes = -offsetMinutes
		}
	default:
		return "", false
	}

	t := time.Date(year, time.Month(month), day, hour, minute, second, 0, time.UTC)
	if t.Year() != year || int(t.Month()) != month || t.Day() != day || t.Hour() != hour || t.Minute() != minute {
		return "", false
	}
	utc := t.Add(time.Duration(-offsetMinutes) * time.Minute)
	if utc.Year() < 0 || utc.Year() > 9999 {
		// The UTC form must stay a four-digit RFC3339 year.
		return "", false
	}
	return utc.Format("2006-01-02T15:04:05") + fraction + "Z", true
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

func writeError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/xjeey8iust/sen-audit-ledger/internal/store"
)

// InvalidAuditInputError rejects a request body that violates the published
// input contract; it maps to HTTP 400 with code invalid_audit_input.
type InvalidAuditInputError struct{ Reason string }

func (e *InvalidAuditInputError) Error() string { return "invalid audit input: " + e.Reason }

// AuditSeqConflictError reports an explicit seq that is already recorded; it
// maps to HTTP 409 with code audit_seq_conflict.
type AuditSeqConflictError struct{ Seq uint64 }

func (e *AuditSeqConflictError) Error() string {
	return fmt.Sprintf("audit seq %d is already recorded", e.Seq)
}

// createEventRequest mirrors the published request shape so the decoder can
// reject unknown fields, including the reserved hash and prev_hash.
type createEventRequest struct {
	Account    *string          `json:"account"`
	Operation  *string          `json:"operation"`
	Resource   *string          `json:"resource"`
	Result     *string          `json:"result"`
	OccurredAt *string          `json:"occurred_at"`
	Seq        *json.RawMessage `json:"seq"`
}

type eventResponse struct {
	Seq        uint64 `json:"seq"`
	Account    string `json:"account"`
	Operation  string `json:"operation"`
	Resource   string `json:"resource"`
	Result     string `json:"result"`
	OccurredAt string `json:"occurred_at"`
	PrevHash   string `json:"prev_hash"`
	Hash       string `json:"hash"`
}

// createEvent handles POST /events.
func createEvent(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		event, err := appendEvent(st, c.Request.Body)
		if err != nil {
			respondEventError(c, err)
			return
		}
		c.JSON(http.StatusCreated, eventResponse{
			Seq:        event.Seq,
			Account:    event.Account,
			Operation:  event.Operation,
			Resource:   event.Resource,
			Result:     event.Result,
			OccurredAt: event.OccurredAt,
			PrevHash:   event.PrevHash,
			Hash:       event.Hash,
		})
	}
}

// appendEvent validates the request body and appends the event to the ledger.
func appendEvent(st *store.Store, body io.Reader) (store.Event, error) {
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	var req createEventRequest
	if err := decoder.Decode(&req); err != nil {
		return store.Event{}, &InvalidAuditInputError{Reason: "body must be a single JSON object with the published fields"}
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return store.Event{}, &InvalidAuditInputError{Reason: "body must contain exactly one JSON value"}
	}

	required := []struct {
		name  string
		value *string
	}{
		{"account", req.Account},
		{"operation", req.Operation},
		{"resource", req.Resource},
		{"result", req.Result},
	}
	for _, field := range required {
		if field.value == nil {
			return store.Event{}, &InvalidAuditInputError{Reason: field.name + " is required and must be a string"}
		}
		if strings.TrimSpace(*field.value) == "" {
			return store.Event{}, &InvalidAuditInputError{Reason: field.name + " must not be blank"}
		}
	}
	if req.OccurredAt == nil {
		return store.Event{}, &InvalidAuditInputError{Reason: "occurred_at is required and must be a string"}
	}
	occurredAt, err := normalizeOccurredAt(*req.OccurredAt)
	if err != nil {
		return store.Event{}, err
	}

	var seq *uint64
	if req.Seq != nil {
		seq, err = parseSeq(*req.Seq)
		if err != nil {
			return store.Event{}, err
		}
	}

	event, err := st.Append(store.EventInput{
		Account:    *req.Account,
		Operation:  *req.Operation,
		Resource:   *req.Resource,
		Result:     *req.Result,
		OccurredAt: occurredAt,
		Seq:        seq,
	})
	switch {
	case errors.Is(err, store.ErrSeqConflict):
		return store.Event{}, &AuditSeqConflictError{Seq: *seq}
	case errors.Is(err, store.ErrSeqNotNext):
		return store.Event{}, &InvalidAuditInputError{Reason: "seq must be the next sequence number"}
	case err != nil:
		return store.Event{}, err
	}
	return event, nil
}

// respondEventError maps an append failure onto the published error shape.
func respondEventError(c *gin.Context, err error) {
	var invalid *InvalidAuditInputError
	var conflict *AuditSeqConflictError
	switch {
	case errors.As(err, &invalid):
		writeError(c, http.StatusBadRequest, "invalid_audit_input", invalid.Error())
	case errors.As(err, &conflict):
		writeError(c, http.StatusConflict, "audit_seq_conflict", conflict.Error())
	default:
		writeError(c, http.StatusServiceUnavailable, "storage_unavailable", "storage is not available")
	}
}

func writeError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}

// occurredAtPattern pins the RFC3339 shape with a mandatory numeric-or-Z
// offset; the fractional-second group is preserved verbatim in the UTC output.
var occurredAtPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$`)

// normalizeOccurredAt validates an RFC3339 timestamp and rewrites it in UTC.
// Leap seconds and every other out-of-range component are rejected.
func normalizeOccurredAt(raw string) (string, error) {
	invalid := &InvalidAuditInputError{Reason: "occurred_at must be an RFC3339 timestamp with a timezone offset"}
	match := occurredAtPattern.FindStringSubmatch(raw)
	if match == nil {
		return "", invalid
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return "", invalid
	}
	return parsed.UTC().Format("2006-01-02T15:04:05") + match[1] + "Z", nil
}

var seqPattern = regexp.MustCompile(`^[0-9]+$`)

// parseSeq accepts only a positive integer literal: no decimals, no
// exponents, no sign, no surrounding JSON string quotes.
func parseSeq(raw json.RawMessage) (*uint64, error) {
	invalid := &InvalidAuditInputError{Reason: "seq must be a positive integer without decimals or exponents"}
	text := strings.TrimSpace(string(raw))
	if !seqPattern.MatchString(text) {
		return nil, invalid
	}
	value, err := strconv.ParseUint(text, 10, 64)
	if err != nil || value == 0 {
		return nil, invalid
	}
	return &value, nil
}

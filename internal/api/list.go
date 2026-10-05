package api

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/xjeey8iust/sen-audit-ledger/internal/store"
)

// Pagination bounds for GET /events.
const (
	defaultListLimit = 50
	maxListLimit     = 100
)

// listResponse is the GET /events body. NextAfterSeq is a pointer so JSON
// renders null when no further records match.
type listResponse struct {
	Events       []eventResponse `json:"events"`
	NextAfterSeq *int64          `json:"next_after_seq"`
}

// getEvents returns one page of audit records. Every parameter is validated
// before storage is touched, so a malformed request can never surface as a
// storage failure.
func getEvents(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		filter, ok := parseEventsQuery(c)
		if !ok {
			return
		}
		events, hasMore, err := st.ListEvents(filter)
		if err != nil {
			writeError(c, http.StatusServiceUnavailable, "storage_unavailable", "storage is not available")
			return
		}
		page := make([]eventResponse, 0, len(events))
		for _, event := range events {
			page = append(page, eventResponse{
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
		var next *int64
		if hasMore && len(events) > 0 {
			last := events[len(events)-1].Seq
			next = &last
		}
		c.JSON(http.StatusOK, listResponse{Events: page, NextAfterSeq: next})
	}
}

// parseEventsQuery validates the query string against the list contract. On
// any violation it writes the invalid_audit_input error and reports false.
func parseEventsQuery(c *gin.Context) (store.EventFilter, bool) {
	var filter store.EventFilter
	invalid := func() (store.EventFilter, bool) {
		writeError(c, http.StatusBadRequest, "invalid_audit_input", "query parameters are not valid")
		return store.EventFilter{}, false
	}

	// ParseQuery reports undecodable percent escapes and stray semicolons;
	// anything it cannot decode rejects the whole request.
	values, err := url.ParseQuery(c.Request.URL.RawQuery)
	if err != nil {
		return invalid()
	}

	filter.Limit = defaultListLimit
	for name, occurrences := range values {
		if len(occurrences) != 1 || occurrences[0] == "" {
			// Repeated parameters and explicit empty values are rejected.
			return invalid()
		}
		value := occurrences[0]
		switch name {
		case "account":
			// The decoded text matches stored accounts byte for byte; a
			// value that is nothing but whitespace can never match a
			// record, so it is rejected instead.
			if strings.TrimSpace(value) == "" {
				return invalid()
			}
			filter.Account = &value
		case "from", "to":
			normalized, ok := normalizeOccurredAt(value)
			if !ok {
				return invalid()
			}
			if name == "from" {
				filter.From, filter.HasFrom = normalized, true
			} else {
				filter.To, filter.HasTo = normalized, true
			}
		case "limit":
			n, ok := parseUintParam(value)
			if !ok || n < 1 || n > maxListLimit {
				return invalid()
			}
			filter.Limit = int(n)
		case "after_seq":
			n, ok := parseUintParam(value)
			if !ok {
				return invalid()
			}
			filter.AfterSeq = n
		default:
			return invalid()
		}
	}

	// Both bounds present: the window is only valid when from is the
	// strictly earlier instant.
	if filter.HasFrom && filter.HasTo && store.CompareInstants(filter.From, filter.To) >= 0 {
		return invalid()
	}
	return filter, true
}

// parseUintParam reads a non-negative int64 written as ASCII decimal digits
// only; leading zeros are accepted, anything else (signs, fractions,
// exponents, overflow) is not.
func parseUintParam(value string) (int64, bool) {
	if !isUintLiteral(value) {
		return 0, false
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

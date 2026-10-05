package api

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/xjeey8iust/sen-audit-ledger/internal/store"
)

// Bounds for the pagination parameters of GET /events.
const (
	defaultEventPageLimit = 50
	maxEventPageLimit     = 100
)

// eventsPageResponse is the GET /events body. NextAfterSeq is a pointer so
// JSON renders null when no further records match.
type eventsPageResponse struct {
	Events       []eventResponse `json:"events"`
	NextAfterSeq *int64          `json:"next_after_seq"`
}

// getEvents lists stored audit records by account, time window and seq
// cursor. Every query parameter is validated before storage is touched, so a
// malformed request can never surface as a storage failure.
func getEvents(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		filter, ok := parseEventsQuery(c)
		if !ok {
			return
		}
		events, err := st.ListEvents(filter)
		if err != nil {
			writeError(c, http.StatusServiceUnavailable, "storage_unavailable", "storage is not available")
			return
		}

		page := events
		var next *int64
		if int64(len(page)) > filter.Limit {
			page = page[:filter.Limit]
			last := page[len(page)-1].Seq
			next = &last
		}
		out := make([]eventResponse, 0, len(page))
		for _, e := range page {
			out = append(out, eventResponse{
				Account:    e.Account,
				Operation:  e.Operation,
				Resource:   e.Resource,
				Result:     e.Result,
				OccurredAt: e.OccurredAt,
				Seq:        e.Seq,
				PrevHash:   e.PrevHash,
				Hash:       e.Hash,
			})
		}
		c.JSON(http.StatusOK, eventsPageResponse{Events: out, NextAfterSeq: next})
	}
}

// parseEventsQuery validates the GET /events query string against the audit
// contract. On any violation it writes the invalid_audit_input error and
// reports false.
func parseEventsQuery(c *gin.Context) (store.EventFilter, bool) {
	invalid := func() (store.EventFilter, bool) {
		writeError(c, http.StatusBadRequest, "invalid_audit_input", "query parameters are not valid")
		return store.EventFilter{}, false
	}

	values, err := url.ParseQuery(c.Request.URL.RawQuery)
	if err != nil {
		// Bad percent-escapes or stray semicolons: the query string does not
		// decode cleanly.
		return invalid()
	}

	filter := store.EventFilter{Limit: defaultEventPageLimit}
	for name, list := range values {
		if len(list) != 1 {
			// A repeated parameter is rejected even when the values agree.
			return invalid()
		}
		value := list[0]
		if value == "" || !utf8.ValidString(value) {
			// An explicit empty value, or text that did not decode to valid
			// UTF-8.
			return invalid()
		}
		switch name {
		case "account":
			// Matching is exact on the decoded text: case matters and the
			// surrounding whitespace is kept, but a whitespace-only value
			// can never name an account.
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
				filter.From = &normalized
			} else {
				filter.To = &normalized
			}
		case "limit":
			n, ok := parseUintParam(value)
			if !ok || n < 1 || n > maxEventPageLimit {
				return invalid()
			}
			filter.Limit = n
		case "after_seq":
			n, ok := parseUintParam(value)
			if !ok {
				return invalid()
			}
			filter.AfterSeq = n
		default:
			// Unknown parameters are rejected.
			return invalid()
		}
	}

	if filter.From != nil && filter.To != nil && store.CompareInstants(*filter.From, *filter.To) >= 0 {
		// The window is only valid when from is strictly earlier than to.
		return invalid()
	}
	return filter, true
}

// parseUintParam parses a pagination parameter that only accepts ASCII
// decimal digits (leading zeros allowed) and must fit a non-negative int64.
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

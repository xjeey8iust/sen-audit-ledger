package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/xjeey8iust/sen-audit-ledger/internal/store"
)

// verifyResponse is the GET /ledger/verify body. FirstInvalidSeq is a pointer
// so JSON renders null when the chain is valid.
type verifyResponse struct {
	Valid           bool   `json:"valid"`
	Checked         int64  `json:"checked"`
	FirstInvalidSeq *int64 `json:"first_invalid_seq"`
}

// getLedgerVerify checks the whole ledger against the chain rules. The request
// takes no parameters: any query string is rejected before touching storage so
// an invalid request can never surface as a storage failure.
func getLedgerVerify(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		// ForceQuery catches a bare trailing "?" whose RawQuery is empty.
		if c.Request.URL.RawQuery != "" || c.Request.URL.ForceQuery {
			writeError(c, http.StatusBadRequest, "invalid_audit_input", "this endpoint does not accept query parameters")
			return
		}

		result, err := st.VerifyChain()
		if err != nil {
			writeError(c, http.StatusServiceUnavailable, "storage_unavailable", "storage is not available")
			return
		}
		c.JSON(http.StatusOK, verifyResponse{
			Valid:           result.Valid,
			Checked:         result.Checked,
			FirstInvalidSeq: result.FirstInvalidSeq,
		})
	}
}

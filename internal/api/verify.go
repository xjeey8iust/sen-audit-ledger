package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/xjeey8iust/sen-audit-ledger/internal/store"
)

// verifyResponse is the 200 body for a completed ledger check.
type verifyResponse struct {
	Valid           bool   `json:"valid"`
	Checked         int64  `json:"checked"`
	FirstInvalidSeq *int64 `json:"first_invalid_seq"`
}

// getLedgerVerify checks the whole persisted chain against the append-time
// hash and linkage rules. The endpoint takes no query parameters.
func getLedgerVerify(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		if len(c.Request.URL.Query()) > 0 {
			writeError(c, http.StatusBadRequest, "invalid_audit_input", "query parameters are not accepted")
			return
		}
		result, err := st.Verify()
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

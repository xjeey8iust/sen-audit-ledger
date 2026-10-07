package store

import "sync/atomic"

// verifyRowHook is a test seam for pacing a verification pass. When non-nil,
// VerifyChain calls it with the seq of each row right after the row is
// scanned and before any chain check runs. The service never installs a
// hook; with a nil hook VerifyChain behaves exactly as before.
var verifyRowHook atomic.Pointer[func(seq int64)]

// SetVerifyRowHook installs hook as the verification row hook and returns a
// function that restores the previous hook. It exists so regression tests
// can observe and pause a verification pass mid-scan; it is not part of the
// service behavior.
func SetVerifyRowHook(hook func(seq int64)) (restore func()) {
	previous := verifyRowHook.Swap(&hook)
	return func() {
		if previous == nil {
			verifyRowHook.Store(nil)
			return
		}
		verifyRowHook.Store(previous)
	}
}

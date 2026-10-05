package store

// readFaultHook is nil outside tests. It is the seam the read-failure
// regression tests use to model a database that is still connected (Ping keeps
// succeeding, writes keep committing) but fails a read partway through.
//
// After every successfully scanned row during a ListEvents or VerifyChain
// pass, the hook is consulted with the 1-based ordinal of that row within the
// current pass. Returning an error fails the read at exactly that point: the
// rows scanned so far stay inside the read transaction and never reach the
// API response. The hook is never consulted by Ping, Append or any write
// path, and ships as a no-op nil in production.
var readFaultHook func(rowsRead int) error

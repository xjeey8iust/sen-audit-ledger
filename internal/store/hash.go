package store

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

// zeroPrevHash links the first record of the chain.
const zeroPrevHash = "0000000000000000000000000000000000000000000000000000000000000000"

// chainHash hashes the compact JSON array
// [seq, account, operation, resource, result, occurred_at, prev_hash] encoded
// as UTF-8, and returns the SHA-256 digest as lowercase hex.
func chainHash(seq uint64, account, operation, resource, result, occurredAt, prevHash string) string {
	var b strings.Builder
	b.WriteByte('[')
	b.WriteString(strconv.FormatUint(seq, 10))
	for _, s := range []string{account, operation, resource, result, occurredAt, prevHash} {
		b.WriteByte(',')
		writeCanonicalJSONString(&b, s)
	}
	b.WriteByte(']')
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// writeCanonicalJSONString writes s as a JSON string that escapes only the
// double quote, the backslash and control characters; control characters use
// lowercase-hex \u00xx sequences.
func writeCanonicalJSONString(b *strings.Builder, s string) {
	const hexdigits = "0123456789abcdef"
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20:
			b.WriteString("\\u00")
			b.WriteByte(hexdigits[r>>4])
			b.WriteByte(hexdigits[r&0x0f])
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
}

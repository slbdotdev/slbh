package id

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// New returns a short, collision-resistant identifier suitable for runtime
// records. It intentionally avoids a dependency on a UUID implementation.
func New(prefix string) string {
	return newWithBytes(prefix, 10)
}

// NewShort returns an identifier with eight random hexadecimal characters
// after the prefix. It is intended for high-volume, user-facing identifiers
// such as runtimes and agents where a small collision risk is acceptable.
func NewShort(prefix string) string {
	return newWithBytes(prefix, 4)
}

func newWithBytes(prefix string, byteCount int) string {
	b := make([]byte, byteCount)
	if _, err := rand.Read(b); err != nil {
		for i := range b {
			b[i] = byte(time.Now().UnixNano() >> (8 * (i % 8)))
		}
		return prefix + "-" + hex.EncodeToString(b)
	}
	return fmt.Sprintf("%s-%x", prefix, b)
}

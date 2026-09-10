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
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%s-%x-%d", prefix, time.Now().UnixNano(), len(b))
	}
	return prefix + "-" + hex.EncodeToString(b[:])
}

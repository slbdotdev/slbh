//go:build !linux

package orgstore

import (
	"fmt"
	"os"
	"sync"
)

var fallbackLock sync.Mutex

// Linux is the deployment target. This process-local fallback keeps other
// targets buildable and serializes goroutines, but does not claim cross-process
// locking where syscall.Flock is unavailable.
func acquireLock(path string) (func() error, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open orgstore lock: %w", err)
	}
	fallbackLock.Lock()
	return func() error {
		fallbackLock.Unlock()
		return file.Close()
	}, nil
}

// Store-level advisory locking. Every mutating operation (put, rm, tag,
// pin, gc) holds the lock so two agent processes writing into the same
// store cannot interleave a gc sweep with a put. Reads are lock-free:
// blobs are immutable and metadata writes are atomic renames.
package store

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// lockPollInterval is how often a blocked acquirer re-tries.
const lockPollInterval = 25 * time.Millisecond

type lock struct {
	path string
}

// acquireLock takes the store lock, polling for up to wait before giving
// up. wait=0 means a single attempt (used by tests to stay deterministic).
func acquireLock(root string, wait time.Duration) (*lock, error) {
	path := filepath.Join(root, "lock")
	deadline := time.Now().Add(wait)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "pid %d\n", os.Getpid())
			f.Close()
			return &lock{path: path}, nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("store: %w", err)
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf(
				"store: %s is locked by another stashd process (remove %s if none is running)",
				root, path)
		}
		time.Sleep(lockPollInterval)
	}
}

// release removes the lock file. Safe to call once per acquisition.
func (l *lock) release() {
	os.Remove(l.path)
}

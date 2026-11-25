package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/bjoernaagaard/aakb/internal/httpx"
)

const (
	privateDirMode         os.FileMode = 0o700
	privateFileMode        os.FileMode = 0o600
	downloadDirMode        os.FileMode = 0o755
	downloadFileMode       os.FileMode = 0o644
	staleLeaseAge                      = 2 * time.Minute
	leaseHeartbeatInterval             = 15 * time.Second
	leasePollInterval                  = 100 * time.Millisecond
)

func leaseDirPath(locksDir, scope, key string) string {
	sum := sha256.Sum256([]byte(scope + "\x00" + key))
	return filepath.Join(locksDir, hex.EncodeToString(sum[:])+".lock")
}

// acquireLease publishes an atomic cross-process lock directory under locksDir
// containing a unique owner marker file. A bounded heartbeat goroutine refreshes
// the marker's mtime every 15s (well under the 2-minute stale expiry) and exits
// before unlock returns.
func acquireLease(ctx context.Context, locksDir, scope, key string) (unlock func(), owned func() bool, err error) {
	if err := os.MkdirAll(locksDir, privateDirMode); err != nil {
		return nil, nil, localIOError(err)
	}
	lockDir := leaseDirPath(locksDir, scope, key)
	pending, err := os.MkdirTemp(locksDir, ".pending-lease-")
	if err != nil {
		return nil, nil, localIOError(err)
	}
	marker := "owner-" + rand.Text()
	prepared := filepath.Join(pending, marker)
	acquired := false
	defer func() {
		if !acquired {
			_ = os.Remove(prepared) //nolint:errcheck // best-effort cleanup of unacquired pending lease
			_ = os.Remove(pending)  //nolint:errcheck // best-effort cleanup of unacquired pending lease
		}
	}()
	if err := os.WriteFile(prepared, nil, privateFileMode); err != nil {
		return nil, nil, localIOError(err)
	}

	ticker := time.NewTicker(leasePollInterval)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		renameErr := os.Rename(pending, lockDir)
		if renameErr == nil {
			acquired = true
			u, o := maintainLease(lockDir, marker)
			return u, o, nil
		}
		info, statErr := os.Lstat(lockDir)
		if statErr == nil && !info.IsDir() {
			return nil, nil, localIOError(renameErr)
		}
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return nil, nil, localIOError(statErr)
		}
		if statErr != nil && !errors.Is(renameErr, os.ErrExist) && !errors.Is(renameErr, os.ErrPermission) && !errors.Is(renameErr, syscall.ENOTEMPTY) {
			return nil, nil, localIOError(renameErr)
		}

		expireLease(lockDir, staleLeaseAge)
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func maintainLease(lockDir, marker string) (unlock func(), owned func() bool) {
	owner := filepath.Join(lockDir, marker)
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		beat := time.NewTicker(leaseHeartbeatInterval)
		defer beat.Stop()
		for {
			select {
			case <-done:
				return
			case now := <-beat.C:
				if err := os.Chtimes(owner, now, now); err != nil {
					return
				}
			}
		}
	}()
	owned = func() bool {
		info, err := os.Lstat(owner)
		return err == nil && info.Mode().IsRegular()
	}
	unlock = func() {
		close(done)
		<-stopped
		if os.Remove(owner) == nil {
			_ = os.Remove(lockDir) //nolint:errcheck // only an empty lock directory can be removed
		}
	}
	return unlock, owned
}

func expireLease(lockDir string, maxAge time.Duration) {
	entries, err := os.ReadDir(lockDir)
	if err != nil {
		return
	}
	switch {
	case len(entries) == 1 && strings.HasPrefix(entries[0].Name(), "owner-"):
		owner := filepath.Join(lockDir, entries[0].Name())
		info, statErr := os.Lstat(owner)
		if statErr != nil || time.Since(info.ModTime()) <= maxAge {
			return
		}
		if err := os.Remove(owner); err != nil {
			return
		}
	case len(entries) != 0:
		return
	default:
		info, statErr := os.Lstat(lockDir)
		if statErr != nil || time.Since(info.ModTime()) <= maxAge {
			return
		}
	}
	_ = os.Remove(lockDir) //nolint:errcheck // stale empty lock directory cleanup
}

func localIOError(err error) error {
	return httpx.WrapError(httpx.CodeInternal, "local_io", "could not read or write local download files", err)
}

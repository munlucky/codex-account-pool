package authbroker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/munlucky/codex-account-pool/internal/profile"
)

const profileWriteLockStaleAfter = 15 * time.Minute

// AcquireProfileWriteLock serializes auth.json writers across the router
// process and the host login worker. The lock is profile-scoped so unrelated
// accounts continue serving requests while one account is being refreshed or
// re-authenticated.
func AcquireProfileWriteLock(ctx context.Context, store *profile.Store, profileID string) (func(), error) {
	if store == nil {
		return nil, errors.New("profile store is required")
	}
	if err := profile.ValidateProfile(profile.Profile{ID: profileID, Provider: profile.ProviderCodex, Isolation: "codex-home"}); err != nil {
		return nil, err
	}
	lockDir := filepath.Join(store.CodexHome(profileID), ".auth-write.lock")
	if err := os.MkdirAll(store.CodexHome(profileID), 0o700); err != nil {
		return nil, fmt.Errorf("create profile home: %w", err)
	}
	for {
		if err := os.Mkdir(lockDir, 0o700); err == nil {
			_ = os.Chtimes(lockDir, time.Now(), time.Now())
			return func() { _ = os.RemoveAll(lockDir) }, nil
		} else if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("acquire profile auth lock: %w", err)
		}

		if info, err := os.Stat(lockDir); err == nil && time.Since(info.ModTime()) > profileWriteLockStaleAfter {
			_ = os.RemoveAll(lockDir)
			continue
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

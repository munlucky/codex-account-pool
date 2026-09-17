package antigravityauth

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/munlucky/codex-account-pool/internal/profile"
)

func TestStoreRoundTripAndPermissions(t *testing.T) {
	root := t.TempDir()
	store := NewStore(profile.NewStore(root))
	want := Credential{AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour).UTC().Truncate(time.Second), ProjectID: "project"}
	if err := store.Save("google-1", want); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load("google-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != want.AccessToken || got.RefreshToken != want.RefreshToken || got.ProjectID != want.ProjectID || !got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Fatalf("credential mismatch: %#v", got)
	}
	info, err := os.Stat(filepath.Join(root, "profiles", profile.ProviderGoogleAntigravity, "google-1", "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("credential file is too permissive: %o", info.Mode().Perm())
	}
}

func TestStoreRejectsProfileTraversal(t *testing.T) {
	store := NewStore(profile.NewStore(t.TempDir()))
	credential := Credential{AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour), ProjectID: "project"}
	if err := store.Save("../escape", credential); err == nil {
		t.Fatal("expected profile traversal to be rejected")
	}
}

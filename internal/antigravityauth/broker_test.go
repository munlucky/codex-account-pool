package antigravityauth

import (
	"context"
	"github.com/munlucky/codex-account-pool/internal/profile"
	"testing"
	"time"
)

func TestAccountSnapshotChecksProviderMembershipAndDoesNotLoadOtherCredentials(t *testing.T) {
	store := profile.NewStore(t.TempDir())
	r, _ := store.Load()
	for _, p := range []profile.Profile{{ID: "google", Provider: profile.ProviderGoogleAntigravity, Isolation: "oauth-state"}, {ID: "no-credentials", Provider: profile.ProviderGoogleAntigravity, Isolation: "oauth-state"}, {ID: "codex", Provider: profile.ProviderCodex, Isolation: "codex-home"}} {
		if err := r.Add(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Save(r); err != nil {
		t.Fatal(err)
	}
	b := NewBroker(store)
	if err := b.CredentialsStore.Save("google", Credential{AccessToken: "test", RefreshToken: "test", ProjectID: "test", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	preferred, ids, err := b.Accounts(context.Background())
	if err != nil || preferred != "google" || len(ids) != 2 {
		t.Fatal(preferred, ids, err)
	}
	if c, err := b.CredentialsFor(context.Background(), "google"); err != nil || c.ProfileID != "google" {
		t.Fatal(c, err)
	}
	for _, id := range []string{"codex", "../google", "missing", "no-credentials"} {
		if _, err := b.CredentialsFor(context.Background(), id); err == nil {
			t.Fatal("invalid account accepted", id)
		}
	}
	r.Profiles = nil
	store.Save(r)
	if _, err := b.CredentialsFor(context.Background(), "google"); err == nil {
		t.Fatal("deleted account still selected")
	}
}

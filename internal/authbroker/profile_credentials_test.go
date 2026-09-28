package authbroker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/munlucky/codex-account-pool/internal/profile"
)

func TestCredentialsForProfileDoesNotChangeActiveProfile(t *testing.T) {
	store := testStore(t, "one", "two")
	now := time.Unix(2_000_000_000, 0)
	writeAuth(t, store, "one", jwt(t, now.Add(time.Hour), "acct-one"), "refresh-one", "acct-one")
	writeAuth(t, store, "two", jwt(t, now.Add(time.Hour), "acct-two"), "refresh-two", "acct-two")

	broker := New(store)
	broker.Now = func() time.Time { return now }
	creds, err := broker.CredentialsForProfile(context.Background(), "two")
	if err != nil {
		t.Fatal(err)
	}
	if creds.ProfileID != "two" {
		t.Fatalf("profile=%s", creds.ProfileID)
	}
	registry, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	active, ok := registry.ActiveProfile(profile.ProviderCodex)
	if !ok || active.ID != "one" {
		t.Fatalf("active=%+v ok=%v", active, ok)
	}
}

func TestProfileWriteLockDoesNotBlockOtherProfile(t *testing.T) {
	store := testStore(t, "one", "two")
	now := time.Unix(2_000_000_000, 0)
	writeAuth(t, store, "one", jwt(t, now.Add(time.Minute), "acct-one"), "refresh-one", "acct-one")
	writeAuth(t, store, "two", jwt(t, now.Add(time.Hour), "acct-two"), "refresh-two", "acct-two")

	release, err := AcquireProfileWriteLock(context.Background(), store, "one")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	broker := New(store)
	broker.Now = func() time.Time { return now }

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	blocked := make(chan error, 1)
	go func() {
		_, err := broker.CredentialsForProfile(ctx, "one")
		blocked <- err
	}()

	started := time.Now()
	creds, err := broker.CredentialsForProfile(context.Background(), "two")
	if err != nil {
		t.Fatal(err)
	}
	if creds.ProfileID != "two" || time.Since(started) > 40*time.Millisecond {
		t.Fatalf("other profile was blocked: creds=%+v elapsed=%s", creds, time.Since(started))
	}
	if err := <-blocked; err == nil || Code(err) != ErrorTemporarilyUnavailable {
		t.Fatalf("blocked profile error=%v code=%s", err, Code(err))
	}
}

func TestRefreshAuthorityRejectionIsReauthRequired(t *testing.T) {
	store := testStore(t, "one")
	now := time.Unix(2_000_000_000, 0)
	writeAuth(t, store, "one", jwt(t, now.Add(time.Minute), "acct-one"), "refresh-one", "acct-one")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	broker := New(store)
	broker.Now = func() time.Time { return now }
	broker.RefreshURL = server.URL
	_, err := broker.CredentialsForProfile(context.Background(), "one")
	if err == nil || Code(err) != ErrorReauthRequired {
		t.Fatalf("err=%v code=%s", err, Code(err))
	}
}

func TestRefreshBadRequestRequiresExplicitOAuthRejection(t *testing.T) {
	tests := []struct {
		name string
		body string
		want ErrorCode
	}{
		{name: "invalid grant", body: `{"error":"invalid_grant"}`, want: ErrorReauthRequired},
		{name: "refresh reused", body: `{"error":{"code":"refresh_token_reused"}}`, want: ErrorReauthRequired},
		{name: "generic bad request", body: `{"error":"invalid_request"}`, want: ErrorTemporarilyUnavailable},
		{name: "non json bad request", body: "bad request", want: ErrorTemporarilyUnavailable},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := testStore(t, "one")
			now := time.Unix(2_000_000_000, 0)
			writeAuth(t, store, "one", jwt(t, now.Add(time.Minute), "acct-one"), "refresh-one", "acct-one")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			broker := New(store)
			broker.Now = func() time.Time { return now }
			broker.RefreshURL = server.URL
			_, err := broker.CredentialsForProfile(context.Background(), "one")
			if err == nil || Code(err) != tc.want {
				t.Fatalf("err=%v code=%s want=%s", err, Code(err), tc.want)
			}
		})
	}
}

func TestValidateProfileAuthRejectsTokenAccountMismatch(t *testing.T) {
	store := testStore(t, "one")
	now := time.Unix(2_000_000_000, 0)
	writeAuth(t, store, "one", jwt(t, now.Add(time.Hour), "acct-different"), "refresh-one", "acct-one")

	broker := New(store)
	broker.Now = func() time.Time { return now }
	_, err := broker.ValidateProfileAuth("one")
	if err == nil || Code(err) != ErrorReauthRequired {
		t.Fatalf("err=%v code=%s", err, Code(err))
	}
}

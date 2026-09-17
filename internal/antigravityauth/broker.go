package antigravityauth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/munlucky/codex-account-pool/internal/profile"
)

const refreshWindow = time.Minute

type Credentials struct {
	ProfileID   string
	AccessToken string
	ProjectID   string
}

type Broker struct {
	Profiles         *profile.Store
	CredentialsStore *Store
	Authenticator    *Authenticator
	Now              func() time.Time
	mu               sync.Mutex
}

func NewBroker(profiles *profile.Store) *Broker {
	store := NewStore(profiles)
	auth := NewAuthenticatorForStore(store)
	return &Broker{Profiles: profiles, CredentialsStore: store, Authenticator: auth, Now: time.Now}
}

func (b *Broker) Credentials(ctx context.Context) (Credentials, error) {
	if b == nil || b.Profiles == nil || b.CredentialsStore == nil {
		return Credentials{}, errors.New("google-antigravity broker is not configured")
	}
	registry, err := b.Profiles.Load()
	if err != nil {
		return Credentials{}, err
	}
	active, ok := registry.ActiveProfile(profile.ProviderGoogleAntigravity)
	if !ok {
		return Credentials{}, errors.New("no active google-antigravity profile; add one with `gpt-codex-router auth add google-antigravity <profile>`")
	}
	return b.credentialsFor(ctx, active.ID, false)
}

func (b *Broker) ForceRefresh(ctx context.Context, profileID string) (Credentials, error) {
	return b.credentialsFor(ctx, profileID, true)
}

// CandidateCredentials returns other usable Antigravity profiles in registry order.
// It never changes the active profile; callers may use it only for a bounded, verified failover.
func (b *Broker) CandidateCredentials(ctx context.Context, excludeProfileID string) []Credentials {
	if b == nil || b.Profiles == nil || b.CredentialsStore == nil {
		return nil
	}
	registry, err := b.Profiles.Load()
	if err != nil {
		return nil
	}
	out := make([]Credentials, 0, len(registry.Profiles))
	for _, candidate := range registry.Profiles {
		if candidate.Provider != profile.ProviderGoogleAntigravity || candidate.ID == excludeProfileID {
			continue
		}
		credentials, err := b.credentialsFor(ctx, candidate.ID, false)
		if err != nil {
			continue
		}
		out = append(out, credentials)
	}
	return out
}

func (b *Broker) credentialsFor(ctx context.Context, profileID string, force bool) (Credentials, error) {
	current, err := b.CredentialsStore.Load(profileID)
	if err != nil {
		return Credentials{}, err
	}
	if !force && current.ExpiresAt.After(b.now().Add(refreshWindow)) {
		return toCredentials(profileID, current), nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	current, err = b.CredentialsStore.Load(profileID)
	if err != nil {
		return Credentials{}, err
	}
	if !force && current.ExpiresAt.After(b.now().Add(refreshWindow)) {
		return toCredentials(profileID, current), nil
	}
	if b.Authenticator == nil {
		return Credentials{}, fmt.Errorf("google-antigravity profile %s requires token refresh; run `gpt-codex-router auth login google-antigravity %s`", profileID, profileID)
	}
	updated, err := b.Authenticator.Refresh(ctx, profileID, current)
	if err != nil {
		return Credentials{}, fmt.Errorf("refresh google-antigravity profile %s: %w; run `gpt-codex-router auth login google-antigravity %s`", profileID, err, profileID)
	}
	return toCredentials(profileID, updated), nil
}

func (b *Broker) Status(profileID string) (bool, time.Time, error) {
	credential, err := b.CredentialsStore.Load(profileID)
	if err != nil {
		return false, time.Time{}, err
	}
	return credential.AccessToken != "" && credential.RefreshToken != "" && credential.ProjectID != "", credential.ExpiresAt, nil
}

func toCredentials(profileID string, c Credential) Credentials {
	return Credentials{ProfileID: profileID, AccessToken: c.AccessToken, ProjectID: c.ProjectID}
}
func (b *Broker) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

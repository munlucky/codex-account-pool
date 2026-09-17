package antigravityauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/munlucky/codex-account-pool/internal/profile"
)

const credentialVersion = 1

type Credential struct {
	Version      int       `json:"version"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	ProjectID    string    `json:"project_id"`
}

type Store struct {
	Profiles *profile.Store
}

func NewStore(profiles *profile.Store) *Store { return &Store{Profiles: profiles} }

func (s *Store) Path(profileID string) string {
	return s.Profiles.ProfileAuthPath(profile.ProviderGoogleAntigravity, profileID)
}

func (s *Store) Load(profileID string) (Credential, error) {
	if s == nil || s.Profiles == nil {
		return Credential{}, errors.New("antigravity credential store is not configured")
	}
	if err := validateCredentialProfileID(profileID); err != nil {
		return Credential{}, err
	}
	data, err := os.ReadFile(s.Path(profileID))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Credential{}, fmt.Errorf("google-antigravity profile %s has no auth state; run `gpt-codex-router auth login google-antigravity %s`", profileID, profileID)
		}
		return Credential{}, fmt.Errorf("read google-antigravity profile %s: %w", profileID, err)
	}
	var credential Credential
	if err := json.Unmarshal(data, &credential); err != nil {
		return Credential{}, fmt.Errorf("decode google-antigravity profile %s: %w", profileID, err)
	}
	if credential.Version != credentialVersion {
		return Credential{}, fmt.Errorf("unsupported google-antigravity credential version %d", credential.Version)
	}
	if credential.AccessToken == "" || credential.RefreshToken == "" || credential.ProjectID == "" {
		return Credential{}, fmt.Errorf("google-antigravity profile %s has incomplete auth state; run `gpt-codex-router auth login google-antigravity %s`", profileID, profileID)
	}
	return credential, nil
}

func (s *Store) Save(profileID string, credential Credential) error {
	if s == nil || s.Profiles == nil {
		return errors.New("antigravity credential store is not configured")
	}
	if err := validateCredentialProfileID(profileID); err != nil {
		return err
	}
	credential.Version = credentialVersion
	if credential.AccessToken == "" || credential.RefreshToken == "" || credential.ProjectID == "" || credential.ExpiresAt.IsZero() {
		return errors.New("google-antigravity credential is incomplete")
	}
	data, err := json.MarshalIndent(credential, "", "  ")
	if err != nil {
		return fmt.Errorf("encode google-antigravity credential: %w", err)
	}
	data = append(data, '\n')
	return atomicWrite(s.Path(profileID), data)
}

func validateCredentialProfileID(profileID string) error {
	return profile.ValidateProfile(profile.Profile{ID: profileID, Provider: profile.ProviderGoogleAntigravity, Isolation: "oauth-state"})
}

func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create google-antigravity auth directory: %w", err)
	}
	temp, err := os.CreateTemp(dir, ".auth-*.tmp")
	if err != nil {
		return fmt.Errorf("create google-antigravity auth temp file: %w", err)
	}
	name := temp.Name()
	defer os.Remove(name)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return fmt.Errorf("secure google-antigravity auth temp file: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return fmt.Errorf("write google-antigravity auth temp file: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("sync google-antigravity auth temp file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close google-antigravity auth temp file: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("replace google-antigravity auth file: %w", err)
	}
	_ = os.Chmod(path, 0o600)
	return nil
}

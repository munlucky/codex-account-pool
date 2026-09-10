package profile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

const (
	ProviderCodex   = "codex"
	registryVersion = 1
)

var profileIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

type Profile struct {
	ID        string `json:"id"`
	Provider  string `json:"provider"`
	Isolation string `json:"isolation"`
}

type Registry struct {
	Version  int               `json:"version"`
	Profiles []Profile         `json:"profiles"`
	Active   map[string]string `json:"active"`
}

type Store struct {
	root string
	path string
}

func DefaultRoot() (string, error) {
	if override := os.Getenv("GPT_CODEX_ROUTER_HOME"); override != "" {
		return filepath.Abs(override)
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config directory: %w", err)
	}
	return filepath.Join(base, "GPTCodexRouter"), nil
}

func NewStore(root string) *Store {
	return &Store{root: root, path: filepath.Join(root, "registry.json")}
}

func (s *Store) Root() string { return s.root }

func (s *Store) Load() (*Registry, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return newRegistry(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read profile registry: %w", err)
	}
	var registry Registry
	if err := json.Unmarshal(data, &registry); err != nil {
		return nil, fmt.Errorf("decode profile registry: %w", err)
	}
	if registry.Version != registryVersion {
		return nil, fmt.Errorf("unsupported profile registry version %d", registry.Version)
	}
	if registry.Active == nil {
		registry.Active = map[string]string{}
	}
	for _, p := range registry.Profiles {
		if err := ValidateProfile(p); err != nil {
			return nil, fmt.Errorf("invalid stored profile %q: %w", p.ID, err)
		}
	}
	return &registry, nil
}

func (s *Store) Save(registry *Registry) error {
	if registry == nil {
		return errors.New("profile registry is nil")
	}
	registry.Version = registryVersion
	if registry.Active == nil {
		registry.Active = map[string]string{}
	}
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return fmt.Errorf("create profile registry directory: %w", err)
	}
	data, err := json.MarshalIndent(registry, "", "  ")
	if err != nil {
		return fmt.Errorf("encode profile registry: %w", err)
	}
	data = append(data, '\n')
	temp, err := os.CreateTemp(s.root, ".registry-*.tmp")
	if err != nil {
		return fmt.Errorf("create registry temp file: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return fmt.Errorf("secure registry temp file: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return fmt.Errorf("write profile registry: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close profile registry: %w", err)
	}
	if err := os.Rename(tempName, s.path); err != nil {
		return fmt.Errorf("replace profile registry: %w", err)
	}
	return nil
}

func (s *Store) CodexHome(profileID string) string {
	return filepath.Join(s.root, "profiles", ProviderCodex, profileID)
}

func (s *Store) CodexAuthPath(profileID string) string {
	return filepath.Join(s.CodexHome(profileID), "auth.json")
}

func (r *Registry) Add(profile Profile) error {
	if err := ValidateProfile(profile); err != nil {
		return err
	}
	if _, ok := r.Find(profile.Provider, profile.ID); ok {
		return fmt.Errorf("profile %s/%s already exists", profile.Provider, profile.ID)
	}
	r.Profiles = append(r.Profiles, profile)
	if r.Active == nil {
		r.Active = map[string]string{}
	}
	if r.Active[profile.Provider] == "" {
		r.Active[profile.Provider] = profile.ID
	}
	return nil
}

func (r *Registry) Use(provider, profileID string) error {
	if _, ok := r.Find(provider, profileID); !ok {
		return fmt.Errorf("profile %s/%s not found", provider, profileID)
	}
	if r.Active == nil {
		r.Active = map[string]string{}
	}
	r.Active[provider] = profileID
	return nil
}

func (r *Registry) Find(provider, profileID string) (Profile, bool) {
	for _, p := range r.Profiles {
		if p.Provider == provider && p.ID == profileID {
			return p, true
		}
	}
	return Profile{}, false
}

func (r *Registry) ActiveProfile(provider string) (Profile, bool) {
	if r.Active == nil {
		return Profile{}, false
	}
	id := r.Active[provider]
	if id == "" {
		return Profile{}, false
	}
	return r.Find(provider, id)
}

func (r *Registry) SortedProfiles() []Profile {
	out := append([]Profile(nil), r.Profiles...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider == out[j].Provider {
			return out[i].ID < out[j].ID
		}
		return out[i].Provider < out[j].Provider
	})
	return out
}

func ValidateProfile(p Profile) error {
	if !profileIDPattern.MatchString(p.ID) {
		return errors.New("profile id must match [A-Za-z0-9][A-Za-z0-9._-]{0,63}")
	}
	switch p.Provider {
	case ProviderCodex:
		if p.Isolation != "codex-home" {
			return errors.New("codex profiles must use codex-home isolation")
		}
	default:
		return fmt.Errorf("unsupported provider %q", p.Provider)
	}
	return nil
}

func newRegistry() *Registry {
	return &Registry{Version: registryVersion, Active: map[string]string{}}
}

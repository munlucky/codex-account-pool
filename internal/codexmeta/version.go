package codexmeta

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const clientVersionFile = "codex-client-version"

var clientVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][A-Za-z0-9.-]+)?$`)

type versionCandidate struct {
	version string
	at      time.Time
}

func ResolveClientVersion(root string) (string, error) {
	if version := strings.TrimSpace(os.Getenv("GPT_CODEX_ROUTER_CODEX_CLIENT_VERSION")); version != "" {
		if !clientVersionPattern.MatchString(version) {
			return "", fmt.Errorf("invalid GPT_CODEX_ROUTER_CODEX_CLIENT_VERSION %q", version)
		}
		return version, nil
	}

	if version, err := readVersionFile(filepath.Join(root, clientVersionFile)); err == nil && version != "" {
		return version, nil
	}

	profilesRoot := filepath.Join(root, "profiles", "codex")
	entries, err := os.ReadDir(profilesRoot)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read Codex profiles for client version: %w", err)
	}
	var candidates []versionCandidate
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidate, ok := readModelsCacheVersion(filepath.Join(profilesRoot, entry.Name(), "models_cache.json"))
		if ok {
			candidates = append(candidates, candidate)
		}
	}
	if len(candidates) > 0 {
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].at.After(candidates[j].at) })
		return candidates[0].version, nil
	}

	return "", errors.New("Codex client version unavailable; rerun setup or start Codex once so a model cache is created")
}

func PersistClientVersion(root, version string) error {
	version = strings.TrimSpace(version)
	if !clientVersionPattern.MatchString(version) {
		return fmt.Errorf("invalid Codex client version %q", version)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("create router state directory: %w", err)
	}
	path := filepath.Join(root, clientVersionFile)
	temp, err := os.CreateTemp(root, ".codex-client-version-*.tmp")
	if err != nil {
		return fmt.Errorf("create Codex client version temp file: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return fmt.Errorf("secure Codex client version temp file: %w", err)
	}
	if _, err := temp.WriteString(version + "\n"); err != nil {
		temp.Close()
		return fmt.Errorf("write Codex client version: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close Codex client version: %w", err)
	}
	if err := os.Rename(tempName, path); err != nil {
		return fmt.Errorf("replace Codex client version: %w", err)
	}
	return nil
}

func readVersionFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	version := strings.TrimSpace(string(data))
	if !clientVersionPattern.MatchString(version) {
		return "", fmt.Errorf("invalid Codex client version file %s", path)
	}
	return version, nil
}

func readModelsCacheVersion(path string) (versionCandidate, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return versionCandidate{}, false
	}
	var cache struct {
		FetchedAt     time.Time `json:"fetched_at"`
		ClientVersion string    `json:"client_version"`
	}
	if json.Unmarshal(data, &cache) != nil {
		return versionCandidate{}, false
	}
	cache.ClientVersion = strings.TrimSpace(cache.ClientVersion)
	if !clientVersionPattern.MatchString(cache.ClientVersion) {
		return versionCandidate{}, false
	}
	return versionCandidate{version: cache.ClientVersion, at: cache.FetchedAt}, true
}

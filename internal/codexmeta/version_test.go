package codexmeta

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveClientVersionPrefersManagedStateFile(t *testing.T) {
	root := t.TempDir()
	if err := PersistClientVersion(root, "0.153.4"); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveClientVersion(root)
	if err != nil {
		t.Fatal(err)
	}
	if got != "0.153.4" {
		t.Fatalf("version=%q", got)
	}
}

func TestResolveClientVersionFallsBackToNewestModelCache(t *testing.T) {
	root := t.TempDir()
	oldHome := filepath.Join(root, "profiles", "codex", "old")
	newHome := filepath.Join(root, "profiles", "codex", "new")
	if err := os.MkdirAll(oldHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(newHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldHome, "models_cache.json"), []byte(`{"fetched_at":"2026-09-08T00:00:00Z","client_version":"0.152.0","models":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newHome, "models_cache.json"), []byte(`{"fetched_at":"2026-09-09T00:00:00Z","client_version":"0.153.4","models":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveClientVersion(root)
	if err != nil {
		t.Fatal(err)
	}
	if got != "0.153.4" {
		t.Fatalf("version=%q", got)
	}
}

func TestResolveClientVersionEnvOverride(t *testing.T) {
	t.Setenv("GPT_CODEX_ROUTER_CODEX_CLIENT_VERSION", "0.200.1")
	got, err := ResolveClientVersion(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got != "0.200.1" {
		t.Fatalf("version=%q", got)
	}
}

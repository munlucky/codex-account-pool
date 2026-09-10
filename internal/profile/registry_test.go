package profile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStoreRoundTripAndActiveProfile(t *testing.T) {
	store := NewStore(t.TempDir())
	registry, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Add(Profile{ID: "work", Provider: ProviderCodex, Isolation: "codex-home"}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Add(Profile{ID: "personal", Provider: ProviderCodex, Isolation: "codex-home"}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Use(ProviderCodex, "personal"); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(registry); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	active, ok := loaded.ActiveProfile(ProviderCodex)
	if !ok || active.ID != "personal" {
		t.Fatalf("active=%+v ok=%v", active, ok)
	}
	if got, want := store.CodexHome("personal"), filepath.Join(store.Root(), "profiles", "codex", "personal"); got != want {
		t.Fatalf("CodexHome=%q want=%q", got, want)
	}
	info, err := os.Stat(filepath.Join(store.Root(), "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.IsDir() {
		t.Fatal("registry.json is a directory")
	}
}

func TestValidateProfileRejectsTraversal(t *testing.T) {
	for _, id := range []string{"../other", "a/b", "", ".hidden"} {
		err := ValidateProfile(Profile{ID: id, Provider: ProviderCodex, Isolation: "codex-home"})
		if err == nil {
			t.Fatalf("expected id %q to be rejected", id)
		}
	}
}

func TestCodexAuthPathLivesInsideProfileHome(t *testing.T) {
	store := NewStore(t.TempDir())
	want := filepath.Join(store.CodexHome("personal"), "auth.json")
	if got := store.CodexAuthPath("personal"); got != want {
		t.Fatalf("CodexAuthPath=%q want=%q", got, want)
	}
}

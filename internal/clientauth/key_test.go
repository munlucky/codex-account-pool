package clientauth

import (
	"os"
	"testing"
)

func TestEnsurePersistsStableKeyAndMatchesBearer(t *testing.T) {
	root := t.TempDir()
	first, err := Ensure(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Ensure(root)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || !Matches("Bearer "+first, first) || Matches("Bearer wrong", first) {
		t.Fatalf("key lifecycle mismatch first=%q second=%q", first, second)
	}
	info, err := os.Stat(Path(root))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("client key permissions too broad: %v", info.Mode().Perm())
	}
}

package clientauth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const fileName = "client-key"

func Path(root string) string {
	return filepath.Join(root, fileName)
}

func Ensure(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errors.New("client auth root is required")
	}
	path := Path(root)
	if key, err := read(path); err == nil {
		return key, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("create client auth directory: %w", err)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate client API key: %w", err)
	}
	key := "gcr_" + base64.RawURLEncoding.EncodeToString(raw)
	temp, err := os.CreateTemp(root, ".client-key-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create client API key temp file: %w", err)
	}
	name := temp.Name()
	defer os.Remove(name)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return "", fmt.Errorf("secure client API key temp file: %w", err)
	}
	if _, err := temp.WriteString(key + "\n"); err != nil {
		temp.Close()
		return "", fmt.Errorf("write client API key: %w", err)
	}
	if err := temp.Close(); err != nil {
		return "", fmt.Errorf("close client API key temp file: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return "", fmt.Errorf("install client API key: %w", err)
	}
	_ = os.Chmod(path, 0o600)
	return key, nil
}

func Load(root string) (string, error) {
	return read(Path(root))
}

func Matches(header, key string) bool {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return false
	}
	candidate := strings.TrimSpace(header[len(prefix):])
	if candidate == "" || key == "" || len(candidate) != len(key) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(key)) == 1
}

func read(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	key := strings.TrimSpace(string(data))
	if !strings.HasPrefix(key, "gcr_") || len(key) < 32 {
		return "", fmt.Errorf("client API key at %s is invalid", path)
	}
	return key, nil
}

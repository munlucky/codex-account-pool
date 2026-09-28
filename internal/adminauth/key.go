package adminauth

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

const (
	adminKeyFile  = "admin-key"
	workerKeyFile = "worker-key"
)

func AdminKeyPath(root string) string  { return filepath.Join(root, adminKeyFile) }
func WorkerKeyPath(root string) string { return filepath.Join(root, workerKeyFile) }

func EnsureAdminKey(root string) (string, error) {
	return ensure(root, adminKeyFile, "gcr_admin_")
}

func EnsureWorkerKey(root string) (string, error) {
	return ensure(root, workerKeyFile, "gcr_worker_")
}

func LoadWorkerKey(root string) (string, error) {
	return read(WorkerKeyPath(root), "gcr_worker_")
}

func MatchesBearer(header, key string) bool {
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

func MatchesKey(candidate, key string) bool {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" || key == "" || len(candidate) != len(key) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(key)) == 1
}

func ensure(root, name, prefix string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errors.New("admin auth root is required")
	}
	path := filepath.Join(root, name)
	if key, err := read(path, prefix); err == nil {
		return key, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("create admin auth directory: %w", err)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate local key: %w", err)
	}
	key := prefix + base64.RawURLEncoding.EncodeToString(raw)
	temp, err := os.CreateTemp(root, "."+name+"-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create local key temp file: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return "", fmt.Errorf("secure local key temp file: %w", err)
	}
	if _, err := temp.WriteString(key + "\n"); err != nil {
		temp.Close()
		return "", fmt.Errorf("write local key: %w", err)
	}
	if err := temp.Close(); err != nil {
		return "", fmt.Errorf("close local key temp file: %w", err)
	}
	if err := os.Rename(tempName, path); err != nil {
		return "", fmt.Errorf("install local key: %w", err)
	}
	_ = os.Chmod(path, 0o600)
	return key, nil
}

func read(path, prefix string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	key := strings.TrimSpace(string(data))
	if !strings.HasPrefix(key, prefix) || len(key) < len(prefix)+32 {
		return "", fmt.Errorf("local key at %s is invalid", path)
	}
	return key, nil
}

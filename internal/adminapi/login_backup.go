package adminapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/munlucky/codex-account-pool/internal/profile"
)

const loginBackupFile = ".gcr-login-backup.json"

type loginBackup struct {
	Version        int    `json:"version"`
	JobID          string `json:"job_id"`
	PreviousExists bool   `json:"previous_exists"`
	PreviousAuth   string `json:"previous_auth,omitempty"`
}

func loginBackupPath(store *profile.Store, profileID string) string {
	return filepath.Join(store.CodexHome(profileID), loginBackupFile)
}

func writeLoginBackup(store *profile.Store, profileID, jobID string, previous []byte, previousExists bool) error {
	path := loginBackupPath(store, profileID)
	if _, err := os.Stat(path); err == nil {
		return errors.New("unresolved login backup already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	backup := loginBackup{Version: 1, JobID: jobID, PreviousExists: previousExists}
	if previousExists {
		backup.PreviousAuth = base64.StdEncoding.EncodeToString(previous)
	}
	data, err := json.Marshal(backup)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return atomicWrite(path, data)
}

func readLoginBackup(store *profile.Store, profileID string) (loginBackup, error) {
	data, err := os.ReadFile(loginBackupPath(store, profileID))
	if err != nil {
		return loginBackup{}, err
	}
	var backup loginBackup
	if err := json.Unmarshal(data, &backup); err != nil {
		return loginBackup{}, err
	}
	if backup.Version != 1 || strings.TrimSpace(backup.JobID) == "" {
		return loginBackup{}, errors.New("invalid login backup metadata")
	}
	if backup.PreviousExists {
		if _, err := base64.StdEncoding.DecodeString(backup.PreviousAuth); err != nil {
			return loginBackup{}, errors.New("invalid login backup payload")
		}
	}
	return backup, nil
}

func clearLoginBackup(store *profile.Store, profileID, jobID string) error {
	backup, err := readLoginBackup(store, profileID)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if backup.JobID != jobID {
		return errors.New("login backup belongs to a different job")
	}
	if err := os.Remove(loginBackupPath(store, profileID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func restoreLoginBackup(store *profile.Store, profileID, jobID string) error {
	backup, err := readLoginBackup(store, profileID)
	if err != nil {
		return err
	}
	if backup.JobID != jobID {
		return errors.New("login backup belongs to a different job")
	}
	var previous []byte
	if backup.PreviousExists {
		previous, err = base64.StdEncoding.DecodeString(backup.PreviousAuth)
		if err != nil {
			return errors.New("invalid login backup payload")
		}
	}
	return restoreAuth(store.CodexAuthPath(profileID), previous, backup.PreviousExists)
}

func readOptional(path string) ([]byte, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

func restoreAuth(path string, previous []byte, existed bool) error {
	if !existed {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return atomicWrite(path, previous)
}

func atomicWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".router-auth-*.tmp")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	_ = os.Chmod(path, 0o600)
	return nil
}

func (s *Service) ReconcileLoginBackups() error {
	if s == nil || s.Profiles == nil || s.State == nil {
		return errors.New("admin service is not configured")
	}
	root := filepath.Join(s.Profiles.Root(), "profiles", profile.ProviderCodex)
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read login backup profiles: %w", err)
	}
	var joined []error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		profileID := entry.Name()
		backup, err := readLoginBackup(s.Profiles, profileID)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			joined = append(joined, fmt.Errorf("read login backup for %s: %w", profileID, err))
			continue
		}
		job, ok := s.State.Job(backup.JobID)
		if !ok {
			continue
		}
		switch job.State {
		case JobSucceeded:
			if err := clearLoginBackup(s.Profiles, profileID, backup.JobID); err != nil {
				joined = append(joined, err)
			}
		case JobFailed, JobCanceled:
			_ = os.RemoveAll(filepath.Join(s.Profiles.CodexHome(profileID), ".auth-write.lock"))
			if err := restoreLoginBackup(s.Profiles, profileID, backup.JobID); err != nil {
				joined = append(joined, fmt.Errorf("restore login backup for %s: %w", profileID, err))
				continue
			}
			if job.NewProfile {
				if err := s.removeNewProfile(profileID); err != nil {
					joined = append(joined, err)
					continue
				}
			}
			if err := clearLoginBackup(s.Profiles, profileID, backup.JobID); err != nil {
				joined = append(joined, err)
			}
		}
	}
	return errors.Join(joined...)
}

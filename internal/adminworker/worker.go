package adminworker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/munlucky/codex-account-pool/internal/adminapi"
	"github.com/munlucky/codex-account-pool/internal/authbroker"
	"github.com/munlucky/codex-account-pool/internal/process"
	"github.com/munlucky/codex-account-pool/internal/profile"
)

const (
	defaultPollInterval = time.Second
	heartbeatInterval   = 5 * time.Second
	loginTimeout        = 10 * time.Minute
	finalizationTimeout = 30 * time.Second
	loginBackupFile     = ".gcr-login-backup.json"
)

var errCompletionRejected = errors.New("login completion rejected")

type Worker struct {
	ServerURL string
	WorkerKey string
	Store     *profile.Store
	Executor  process.Executor
	CodexBin  string
	Out       io.Writer
	Err       io.Writer

	Client       *http.Client
	PollInterval time.Duration
}

type leaseResponse struct {
	JobID      string    `json:"job_id"`
	Provider   string    `json:"provider"`
	ProfileID  string    `json:"profile_id"`
	NewProfile bool      `json:"new_profile"`
	LeaseID    string    `json:"lease_id"`
	LeaseUntil time.Time `json:"lease_until"`
}

type loginBackup struct {
	Version        int    `json:"version"`
	JobID          string `json:"job_id"`
	PreviousExists bool   `json:"previous_exists"`
	PreviousAuth   string `json:"previous_auth,omitempty"`
}

func (w *Worker) Run(ctx context.Context, once bool) error {
	if err := w.validate(); err != nil {
		return err
	}
	var lastDiagnostics time.Time
	for {
		if lastDiagnostics.IsZero() || time.Since(lastDiagnostics) >= 30*time.Second {
			if err := w.reportDiagnostics(ctx); err == nil {
				lastDiagnostics = time.Now()
			}
		}
		job, ok, err := w.lease(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if once {
				return err
			}
			if w.Err != nil {
				fmt.Fprintf(w.Err, "host worker cannot reach router: %v\n", err)
			}
			interval := w.PollInterval
			if interval <= 0 {
				interval = defaultPollInterval
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(interval):
				continue
			}
		}
		if err := w.reconcileLoginBackups(ctx); err != nil && w.Err != nil {
			fmt.Fprintf(w.Err, "host worker backup reconciliation failed: %v\n", err)
		}
		if ok {
			if err := w.runLogin(ctx, job); err != nil && w.Err != nil {
				fmt.Fprintf(w.Err, "host worker login job %s failed: %v\n", job.JobID, err)
			}
			if once {
				return nil
			}
			continue
		}
		if once {
			return nil
		}
		interval := w.PollInterval
		if interval <= 0 {
			interval = defaultPollInterval
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

func (w *Worker) validate() error {
	if w.Store == nil || w.Executor == nil {
		return errors.New("host worker requires profile store and executor")
	}
	if strings.TrimSpace(w.WorkerKey) == "" {
		return errors.New("host worker key is required")
	}
	if strings.TrimSpace(w.CodexBin) == "" {
		w.CodexBin = "codex"
	}
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(w.ServerURL), "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return errors.New("host worker server URL is invalid")
	}
	host := parsed.Hostname()
	if !strings.EqualFold(host, "localhost") {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return errors.New("host worker refuses a non-loopback server URL")
		}
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("host worker server URL must use http or https")
	}
	w.ServerURL = strings.TrimRight(parsed.String(), "/")
	if w.Client == nil {
		w.Client = &http.Client{Timeout: 10 * time.Second}
	}
	return nil
}

func (w *Worker) lease(ctx context.Context) (leaseResponse, bool, error) {
	var job leaseResponse
	status, err := w.postJSON(ctx, "/admin/worker/lease", struct{}{}, &job)
	if err != nil {
		return job, false, err
	}
	if status == http.StatusNoContent {
		return job, false, nil
	}
	if status != http.StatusOK {
		return job, false, fmt.Errorf("worker lease returned HTTP %d", status)
	}
	if job.JobID == "" || job.LeaseID == "" || job.Provider != profile.ProviderCodex {
		return job, false, errors.New("worker lease response is invalid")
	}
	if err := profile.ValidateProfile(profile.Profile{ID: job.ProfileID, Provider: profile.ProviderCodex, Isolation: "codex-home"}); err != nil {
		return job, false, errors.New("worker lease profile is invalid")
	}
	return job, true, nil
}

func (w *Worker) runLogin(parent context.Context, job leaseResponse) error {
	lockCtx, lockCancel := context.WithTimeout(parent, 30*time.Second)
	release, err := authbroker.AcquireProfileWriteLock(lockCtx, w.Store, job.ProfileID)
	lockCancel()
	if err != nil {
		_ = w.complete(parent, job, false, adminapi.ErrWriteFailed)
		return err
	}

	authPath := w.Store.CodexAuthPath(job.ProfileID)
	previous, previousExists, err := readOptional(authPath)
	if err != nil {
		release()
		_ = w.complete(parent, job, false, adminapi.ErrWriteFailed)
		return err
	}
	oldAccountID := accountIDFromAuth(previous)
	if err := writeLoginBackup(w.Store, job.ProfileID, job.JobID, previous, previousExists); err != nil {
		release()
		_ = w.complete(parent, job, false, adminapi.ErrWriteFailed)
		return err
	}

	if err := os.MkdirAll(w.Store.CodexHome(job.ProfileID), 0o700); err != nil {
		_ = clearLoginBackup(w.Store, job.ProfileID, job.JobID)
		release()
		_ = w.complete(parent, job, false, adminapi.ErrWriteFailed)
		return err
	}

	loginCtx, cancel := context.WithTimeout(parent, loginTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- w.Executor.Run(loginCtx, process.Command{
			Executable: w.CodexBin,
			Args:       []string{"-c", `cli_auth_credentials_store="file"`, "login"},
			SetEnv:     map[string]string{"CODEX_HOME": filepath.Clean(w.Store.CodexHome(job.ProfileID))},
			UnsetEnv:   []string{"OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN"},
			Stdout:     io.Discard,
			Stderr:     io.Discard,
		})
	}()

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	execErr := error(nil)
	leaseLost := false
wait:
	for {
		select {
		case execErr = <-done:
			break wait
		case <-ticker.C:
			if err := w.heartbeat(loginCtx, job); err != nil {
				leaseLost = true
				cancel()
				execErr = <-done
				break wait
			}
		case <-loginCtx.Done():
			execErr = <-done
			break wait
		}
	}

	errorCode := ""
	if execErr != nil || loginCtx.Err() != nil {
		switch {
		case leaseLost:
			errorCode = adminapi.ErrCanceled
		case errors.Is(loginCtx.Err(), context.DeadlineExceeded):
			errorCode = adminapi.ErrTimeout
		case errors.Is(loginCtx.Err(), context.Canceled):
			errorCode = adminapi.ErrCanceled
		default:
			errorCode = adminapi.ErrLoginFailed
		}
		if restoreErr := restoreAuthAndClearBackup(w.Store, job.ProfileID, job.JobID, previous, previousExists); restoreErr != nil {
			errorCode = adminapi.ErrWriteFailed
		}
		release()
		if !leaseLost {
			_ = w.complete(parent, job, false, errorCode)
		}
		if execErr != nil {
			return execErr
		}
		return loginCtx.Err()
	}

	current, currentExists, readErr := readOptional(authPath)
	if readErr != nil || !currentExists {
		_ = restoreAuthAndClearBackup(w.Store, job.ProfileID, job.JobID, previous, previousExists)
		release()
		_ = w.complete(parent, job, false, adminapi.ErrAuthFileInvalid)
		if readErr != nil {
			return readErr
		}
		return errors.New("Codex login did not create auth.json")
	}
	newAccountID, valid := validateAuth(current)
	if !valid {
		_ = restoreAuthAndClearBackup(w.Store, job.ProfileID, job.JobID, previous, previousExists)
		release()
		_ = w.complete(parent, job, false, adminapi.ErrAuthFileInvalid)
		return errors.New("Codex login produced an invalid auth file")
	}
	if oldAccountID != "" && newAccountID != oldAccountID {
		_ = restoreAuthAndClearBackup(w.Store, job.ProfileID, job.JobID, previous, previousExists)
		release()
		_ = w.complete(parent, job, false, adminapi.ErrAccountMismatch)
		return errors.New("Codex login account identity does not match the existing profile")
	}

	release()
	if err := w.complete(parent, job, true, ""); err != nil {
		if errors.Is(err, errCompletionRejected) {
			restoreCtx, cancelRestore := context.WithTimeout(context.Background(), 30*time.Second)
			restoreRelease, lockErr := authbroker.AcquireProfileWriteLock(restoreCtx, w.Store, job.ProfileID)
			cancelRestore()
			if lockErr != nil {
				return errors.Join(err, fmt.Errorf("restore lock after rejected completion: %w", lockErr))
			}
			restoreErr := restoreAuthAndClearBackup(w.Store, job.ProfileID, job.JobID, previous, previousExists)
			restoreRelease()
			if restoreErr != nil {
				return errors.Join(err, fmt.Errorf("restore previous auth after rejected completion: %w", restoreErr))
			}
		}
		return err
	}
	if err := w.settleLoginBackup(parent, job); err != nil {
		return err
	}
	if w.Out != nil {
		fmt.Fprintf(w.Out, "Completed Codex login job %s for profile %s.\n", job.JobID, job.ProfileID)
	}
	return nil
}

func (w *Worker) heartbeat(ctx context.Context, job leaseResponse) error {
	var response struct {
		State string `json:"state"`
	}
	status, err := w.postJSON(ctx, "/admin/worker/jobs/"+url.PathEscape(job.JobID)+"/heartbeat", map[string]string{
		"lease_id": job.LeaseID,
	}, &response)
	if err != nil {
		return err
	}
	if status != http.StatusOK || response.State != adminapi.JobLoggingIn {
		return errors.New("login job lease is no longer active")
	}
	return nil
}

func (w *Worker) complete(ctx context.Context, job leaseResponse, success bool, errorCode string) error {
	var response struct {
		State string `json:"state"`
	}
	status, err := w.postJSON(ctx, "/admin/worker/jobs/"+url.PathEscape(job.JobID)+"/complete", map[string]any{
		"lease_id": job.LeaseID, "success": success, "error_code": errorCode,
	}, &response)
	if err != nil {
		return err
	}
	if status == http.StatusConflict {
		return fmt.Errorf("%w: HTTP %d", errCompletionRejected, status)
	}
	if status != http.StatusOK {
		return fmt.Errorf("worker completion returned HTTP %d", status)
	}
	if success && response.State != adminapi.JobLoggingIn && response.State != adminapi.JobSucceeded {
		return fmt.Errorf("%w: server state %s", errCompletionRejected, response.State)
	}
	return nil
}

func (w *Worker) reportDiagnostics(ctx context.Context) error {
	state := desktopRoutingState()
	status, err := w.postJSON(ctx, "/admin/worker/diagnostics", map[string]string{
		"desktop_routing_state": state,
	}, nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent {
		return fmt.Errorf("worker diagnostics returned HTTP %d", status)
	}
	return nil
}

func (w *Worker) postJSON(ctx context.Context, path string, input, output any) (int, error) {
	body, err := json.Marshal(input)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.ServerURL+path, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+w.WorkerKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.Client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if output != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(output); err != nil {
			return resp.StatusCode, err
		}
	} else {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	}
	return resp.StatusCode, nil
}

func (w *Worker) jobStatus(ctx context.Context, jobID string) (adminapi.PublicJob, bool, error) {
	var job adminapi.PublicJob
	status, err := w.postJSON(ctx, "/admin/worker/jobs/"+url.PathEscape(jobID)+"/status", struct{}{}, &job)
	if err != nil {
		return adminapi.PublicJob{}, false, err
	}
	if status == http.StatusNotFound {
		return adminapi.PublicJob{}, false, nil
	}
	if status != http.StatusOK {
		return adminapi.PublicJob{}, false, fmt.Errorf("worker job status returned HTTP %d", status)
	}
	return job, true, nil
}

func (w *Worker) settleLoginBackup(parent context.Context, lease leaseResponse) error {
	ctx, cancel := context.WithTimeout(parent, finalizationTimeout)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()

	for {
		job, exists, err := w.jobStatus(ctx, lease.JobID)
		if err != nil {
			return fmt.Errorf("wait for login finalization: %w", err)
		}
		if !exists {
			return fmt.Errorf("wait for login finalization: job %s is unavailable", lease.JobID)
		}
		switch job.State {
		case adminapi.JobSucceeded:
			return clearLoginBackup(w.Store, lease.ProfileID, lease.JobID)
		case adminapi.JobFailed, adminapi.JobCanceled:
			restoreCtx, restoreCancel := context.WithTimeout(context.Background(), 30*time.Second)
			restoreErr := restoreLoginBackupFile(restoreCtx, w.Store, lease.ProfileID, lease.JobID)
			restoreCancel()
			if restoreErr != nil {
				return errors.Join(fmt.Errorf("login job finalized as %s", job.State), restoreErr)
			}
			return fmt.Errorf("login job finalized as %s", job.State)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (w *Worker) reconcileLoginBackups(ctx context.Context) error {
	pattern := filepath.Join(w.Store.Root(), "profiles", profile.ProviderCodex, "*", loginBackupFile)
	paths, err := filepath.Glob(pattern)
	if err != nil {
		return err
	}
	var joined []error
	for _, path := range paths {
		profileID := filepath.Base(filepath.Dir(path))
		if err := profile.ValidateProfile(profile.Profile{ID: profileID, Provider: profile.ProviderCodex, Isolation: "codex-home"}); err != nil {
			joined = append(joined, fmt.Errorf("invalid backup profile: %w", err))
			continue
		}
		backup, err := readLoginBackup(w.Store, profileID)
		if err != nil {
			joined = append(joined, fmt.Errorf("read login backup for %s: %w", profileID, err))
			continue
		}
		job, exists, err := w.jobStatus(ctx, backup.JobID)
		if err != nil {
			joined = append(joined, fmt.Errorf("query backup job %s: %w", backup.JobID, err))
			continue
		}
		if !exists {
			// Do not guess whether an unavailable historical job succeeded.
			// Keeping the backup is safer than overwriting the current auth.
			continue
		}
		switch job.State {
		case adminapi.JobSucceeded:
			if err := clearLoginBackup(w.Store, profileID, backup.JobID); err != nil {
				joined = append(joined, err)
			}
		case adminapi.JobFailed, adminapi.JobCanceled:
			restoreCtx, restoreCancel := context.WithTimeout(context.Background(), 30*time.Second)
			err := restoreLoginBackupFile(restoreCtx, w.Store, profileID, backup.JobID)
			restoreCancel()
			if err != nil {
				joined = append(joined, err)
			}
		}
	}
	return errors.Join(joined...)
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
	backup := loginBackup{
		Version: 1, JobID: jobID, PreviousExists: previousExists,
	}
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

func restoreLoginBackupFile(ctx context.Context, store *profile.Store, profileID, jobID string) error {
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
	release, err := authbroker.AcquireProfileWriteLock(ctx, store, profileID)
	if err != nil {
		return err
	}
	restoreErr := restoreAuth(store.CodexAuthPath(profileID), previous, backup.PreviousExists)
	release()
	if restoreErr != nil {
		return restoreErr
	}
	return clearLoginBackup(store, profileID, jobID)
}

func restoreAuthAndClearBackup(store *profile.Store, profileID, jobID string, previous []byte, previousExists bool) error {
	if err := restoreAuth(store.CodexAuthPath(profileID), previous, previousExists); err != nil {
		return err
	}
	return clearLoginBackup(store, profileID, jobID)
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
	temp, err := os.CreateTemp(filepath.Dir(path), ".worker-auth-*.tmp")
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

func accountIDFromAuth(data []byte) string {
	id, _ := validateAuth(data)
	return id
}

func validateAuth(data []byte) (string, bool) {
	var document struct {
		AuthMode string `json:"auth_mode"`
		Tokens   struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			AccountID    string `json:"account_id"`
		} `json:"tokens"`
	}
	if json.Unmarshal(data, &document) != nil || !strings.EqualFold(strings.TrimSpace(document.AuthMode), "chatgpt") {
		return "", false
	}
	accountID := strings.TrimSpace(document.Tokens.AccountID)
	if strings.TrimSpace(document.Tokens.AccessToken) == "" || strings.TrimSpace(document.Tokens.RefreshToken) == "" || accountID == "" {
		return "", false
	}
	return accountID, true
}

func desktopRoutingState() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "unknown"
	}
	data, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if errors.Is(err, os.ErrNotExist) {
		return "disabled"
	}
	if err != nil {
		return "unknown"
	}
	want := map[string]string{
		"chatgpt_base_url": "http://127.0.0.1:8317/backend-api",
		"openai_base_url":  "http://127.0.0.1:8317/backend-api/codex",
	}
	found := map[string]bool{}
	for _, raw := range strings.Split(string(data), string([]byte{10})) {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			break
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if expected, ok := want[key]; ok && value == expected {
			found[key] = true
		}
	}
	if found["chatgpt_base_url"] && found["openai_base_url"] {
		return "configured"
	}
	return "disabled"
}

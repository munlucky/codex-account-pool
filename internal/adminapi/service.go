package adminapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/munlucky/codex-account-pool/internal/authbroker"
	"github.com/munlucky/codex-account-pool/internal/codexlogin"
	"github.com/munlucky/codex-account-pool/internal/gateway"
	"github.com/munlucky/codex-account-pool/internal/profile"
)

const (
	checkTimeout  = 20 * time.Second
	checkCooldown = 2 * time.Second
	loginTimeout  = 10 * time.Minute
)

type CheckResult struct {
	Status    string
	ErrorCode string
	CheckedAt time.Time
}

type ProfileChecker interface {
	Check(context.Context, string) CheckResult
}

type CodexChecker struct {
	Broker        *authbroker.Broker
	Gateway       *gateway.Handler
	ClientVersion string
	Now           func() time.Time
}

func (c *CodexChecker) Check(ctx context.Context, profileID string) CheckResult {
	now := time.Now
	if c != nil && c.Now != nil {
		now = c.Now
	}
	result := CheckResult{Status: StatusTemporarilyUnavailable, ErrorCode: ErrTemporary, CheckedAt: now().UTC()}
	if c == nil || c.Broker == nil || c.Gateway == nil || strings.TrimSpace(c.ClientVersion) == "" {
		return result
	}
	if _, err := c.Broker.CredentialsForProfile(ctx, profileID); err != nil {
		switch authbroker.Code(err) {
		case authbroker.ErrorNotLoggedIn:
			result.Status = StatusNotLoggedIn
			result.ErrorCode = ErrNotLoggedIn
		case authbroker.ErrorReauthRequired:
			result.Status = StatusReauthRequired
			result.ErrorCode = ErrReauthRequired
		default:
			result.Status = StatusTemporarilyUnavailable
			result.ErrorCode = ErrTemporary
		}
		return result
	}

	reqURL := &url.URL{Scheme: "http", Host: "router.local", Path: "/backend-api/codex/models"}
	query := reqURL.Query()
	query.Set("client_version", strings.TrimSpace(c.ClientVersion))
	reqURL.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL.String(), nil)
	if err != nil {
		return result
	}
	req.Header.Set("originator", "codex_cli_rs")
	req.Header.Set("version", strings.TrimSpace(c.ClientVersion))
	req.Header.Del("Accept-Encoding")
	capture := newStatusCapture()
	c.Gateway.ServeHTTPForProfile(capture, req, profileID)

	switch capture.status {
	case http.StatusOK:
		result.Status = StatusConnected
		result.ErrorCode = ""
	case http.StatusUnauthorized, http.StatusForbidden:
		result.Status = StatusReauthRequired
		result.ErrorCode = ErrReauthRequired
	default:
		result.Status = StatusTemporarilyUnavailable
		result.ErrorCode = ErrTemporary
	}
	return result
}

type statusCapture struct {
	header http.Header
	status int
}

func newStatusCapture() *statusCapture       { return &statusCapture{header: make(http.Header)} }
func (w *statusCapture) Header() http.Header { return w.header }
func (w *statusCapture) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}
func (w *statusCapture) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return io.Discard.Write(p)
}

type Service struct {
	Profiles    *profile.Store
	Broker      *authbroker.Broker
	State       *StateStore
	Checker     ProfileChecker
	LoginRunner codexlogin.Runner
	Now         func() time.Time

	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}

func NewService(profiles *profile.Store, broker *authbroker.Broker, state *StateStore, checker ProfileChecker) *Service {
	return &Service{
		Profiles: profiles, Broker: broker, State: state, Checker: checker,
		Now: time.Now, cancels: map[string]context.CancelFunc{},
	}
}

func (s *Service) now() time.Time {
	if s != nil && s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) ListProfiles() (ProfileListResponse, error) {
	if s == nil || s.Profiles == nil || s.Broker == nil || s.State == nil {
		return ProfileListResponse{}, errors.New("admin service is not configured")
	}
	registry, err := s.Profiles.Load()
	if err != nil {
		return ProfileListResponse{}, err
	}
	response := ProfileListResponse{ActiveJobs: s.State.ActiveJobs()}
	for _, item := range registry.SortedProfiles() {
		if item.Provider != profile.ProviderCodex {
			continue
		}
		view := ProfileView{
			Provider: item.Provider, ID: item.ID,
			Active: registry.Active[item.Provider] == item.ID,
			Status: StatusUnverified,
		}
		info, inspectErr := s.Broker.InspectProfile(item.ID)
		if inspectErr != nil {
			switch authbroker.Code(inspectErr) {
			case authbroker.ErrorNotLoggedIn:
				view.Status = StatusNotLoggedIn
			case authbroker.ErrorReauthRequired:
				view.Status = StatusReauthRequired
				view.ErrorCode = ErrReauthRequired
			default:
				view.Status = StatusTemporarilyUnavailable
				view.ErrorCode = ErrTemporary
			}
		} else {
			view.HasRefreshToken = info.HasRefreshToken
			if !info.AccessExpiresAt.IsZero() {
				expires := info.AccessExpiresAt.UTC()
				view.AccessExpiresAt = &expires
			}
			switch {
			case !info.HasAuth || !info.HasAccessToken || !info.HasAccountID:
				view.Status = StatusNotLoggedIn
			case !info.HasRefreshToken:
				view.Status = StatusReauthRequired
				view.ErrorCode = ErrReauthRequired
			case info.AccessExpired:
				view.Status = StatusAccessExpiredUnverified
			default:
				view.Status = StatusUnverified
			}
		}
		if active, ok := s.State.ActiveJob(item.Provider, item.ID); ok {
			view.JobID = active.ID
			if active.Type == JobTypeLogin {
				view.Status = StatusLoggingIn
			} else {
				view.Status = StatusChecking
			}
		} else if record, ok := s.State.Check(item.Provider, item.ID); ok {
			checked := record.CheckedAt.UTC()
			view.LastCheckedAt = &checked
			switch record.Status {
			case StatusReauthRequired, StatusTemporarilyUnavailable:
				view.Status = record.Status
				view.ErrorCode = record.ErrorCode
			case StatusConnected:
				if view.Status != StatusNotLoggedIn && view.Status != StatusReauthRequired && view.Status != StatusAccessExpiredUnverified {
					view.Status = StatusConnected
					view.ErrorCode = ""
				}
			}
		}
		response.Profiles = append(response.Profiles, view)
	}
	return response, nil
}

func (s *Service) StartCheck(providerName, profileID string) (Job, error) {
	if providerName != profile.ProviderCodex {
		return Job{}, fmt.Errorf("unsupported provider %q", providerName)
	}
	registry, err := s.Profiles.Load()
	if err != nil {
		return Job{}, err
	}
	if _, ok := registry.Find(providerName, profileID); !ok {
		return Job{}, fmt.Errorf("profile %s/%s not found", providerName, profileID)
	}
	if job, ok := s.State.ActiveJob(providerName, profileID); ok {
		return Job{}, fmt.Errorf("profile operation already active: %s", job.ID)
	}
	if recent, ok := s.State.RecentJob(JobTypeCheck, providerName, profileID); ok && s.now().Sub(recent.CreatedAt) < checkCooldown {
		return Job{}, errors.New("profile check is rate limited")
	}
	job, err := s.State.CreateJob(JobTypeCheck, providerName, profileID, false)
	if err != nil {
		return Job{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
	s.setCancel(job.ID, cancel)
	go s.runCheck(ctx, job)
	return job, nil
}

func (s *Service) runCheck(ctx context.Context, job Job) {
	defer s.clearCancel(job.ID)
	if s.Checker == nil {
		_ = s.State.CompleteCheck(job.ID, CheckRecord{
			Status: StatusTemporarilyUnavailable, ErrorCode: ErrTemporary, CheckedAt: s.now().UTC(),
		})
		return
	}
	result := s.Checker.Check(ctx, job.ProfileID)
	if errors.Is(ctx.Err(), context.Canceled) {
		return
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		result = CheckResult{Status: StatusTemporarilyUnavailable, ErrorCode: ErrTimeout, CheckedAt: s.now().UTC()}
	}
	_ = s.State.CompleteCheck(job.ID, CheckRecord{
		Status: result.Status, ErrorCode: result.ErrorCode, CheckedAt: result.CheckedAt,
	})
}

func (s *Service) StartLogin(providerName, profileID string) (Job, error) {
	if providerName != profile.ProviderCodex {
		return Job{}, fmt.Errorf("unsupported provider %q", providerName)
	}
	if s.LoginRunner == nil {
		return Job{}, errors.New("container login runtime is unavailable")
	}
	candidate := profile.Profile{ID: profileID, Provider: profile.ProviderCodex, Isolation: "codex-home"}
	if err := profile.ValidateProfile(candidate); err != nil {
		return Job{}, err
	}
	registry, err := s.Profiles.Load()
	if err != nil {
		return Job{}, err
	}
	_, exists := registry.Find(providerName, profileID)
	job, err := s.State.CreateLoginJob(providerName, profileID, !exists)
	if err != nil {
		return Job{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), loginTimeout)
	s.setCancel(job.ID, cancel)
	go s.runLogin(ctx, job)
	return job, nil
}

func (s *Service) runLogin(ctx context.Context, job Job) {
	defer s.clearCancel(job.ID)

	release, err := authbroker.AcquireProfileWriteLock(ctx, s.Profiles, job.ProfileID)
	if err != nil {
		s.failLogin(job, mapLoginRuntimeError(ctx, err))
		return
	}
	locked := true
	defer func() {
		if locked {
			release()
		}
	}()

	authPath := s.Profiles.CodexAuthPath(job.ProfileID)
	previous, previousExists, err := readOptional(authPath)
	if err != nil {
		s.failLogin(job, ErrWriteFailed)
		return
	}
	oldAccountID := ""
	if !job.NewProfile {
		if info, _ := s.Broker.InspectProfile(job.ProfileID); info.AccountID != "" {
			oldAccountID = info.AccountID
		}
	}
	if err := writeLoginBackup(s.Profiles, job.ProfileID, job.ID, previous, previousExists); err != nil {
		s.failLogin(job, ErrWriteFailed)
		return
	}

	err = s.LoginRunner.Login(ctx, s.Profiles.CodexHome(job.ProfileID), func(challenge codexlogin.Challenge) error {
		if !validVerificationURL(challenge.VerificationURL) || strings.TrimSpace(challenge.UserCode) == "" {
			return codexlogin.ErrProtocol
		}
		_, err := s.State.SetLoginChallenge(job.ID, challenge.VerificationURL, challenge.UserCode)
		return err
	})
	if err != nil {
		errorCode := s.restoreBeforeFinalization(job, previous, previousExists, mapLoginRuntimeError(ctx, err))
		s.failLogin(job, errorCode)
		return
	}

	info, err := s.Broker.ValidateProfileAuth(job.ProfileID)
	if err != nil {
		errorCode := s.restoreBeforeFinalization(job, previous, previousExists, mapAuthError(err))
		s.failLogin(job, errorCode)
		return
	}
	if oldAccountID != "" && info.AccountID != oldAccountID {
		errorCode := s.restoreBeforeFinalization(job, previous, previousExists, ErrAccountMismatch)
		s.failLogin(job, errorCode)
		return
	}

	if _, err := s.State.BeginLoginFinalization(job.ID); err != nil {
		errorCode := s.restoreBeforeFinalization(job, previous, previousExists, ErrCanceled)
		s.failLogin(job, errorCode)
		return
	}
	if job.NewProfile {
		if err := s.registerNewProfile(job.ProfileID); err != nil {
			errorCode := s.restoreBeforeFinalization(job, previous, previousExists, ErrWriteFailed)
			_, _ = s.State.FinishLogin(job.ID, false, errorCode)
			return
		}
	}

	// The app-server is done writing auth.json. Release the profile lock before
	// the exact upstream check because a 401 check may perform one token refresh.
	release()
	locked = false

	checkCtx, cancel := context.WithTimeout(context.Background(), checkTimeout)
	check := CheckResult{Status: StatusTemporarilyUnavailable, ErrorCode: ErrTemporary, CheckedAt: s.now().UTC()}
	if s.Checker != nil {
		check = s.Checker.Check(checkCtx, job.ProfileID)
	}
	if errors.Is(checkCtx.Err(), context.DeadlineExceeded) {
		check = CheckResult{Status: StatusTemporarilyUnavailable, ErrorCode: ErrTimeout, CheckedAt: s.now().UTC()}
	}
	cancel()
	if check.Status == StatusConnected {
		if _, err := s.State.FinishLogin(job.ID, true, ""); err != nil {
			return
		}
		_ = s.State.RecordCheck(profile.ProviderCodex, job.ProfileID, CheckRecord{
			Status: check.Status, ErrorCode: check.ErrorCode, CheckedAt: check.CheckedAt,
		})
		_ = clearLoginBackup(s.Profiles, job.ProfileID, job.ID)
		return
	}

	errorCode := check.ErrorCode
	if errorCode == "" {
		errorCode = ErrTemporary
	}
	if err := s.rollbackLogin(job, previous, previousExists); err != nil {
		errorCode = ErrWriteFailed
	}
	_, _ = s.State.FinishLogin(job.ID, false, errorCode)
}

func (s *Service) restoreBeforeFinalization(job Job, previous []byte, previousExists bool, errorCode string) string {
	if err := restoreAuth(s.Profiles.CodexAuthPath(job.ProfileID), previous, previousExists); err != nil {
		// Keep the persistent backup so startup reconciliation can retry.
		return ErrWriteFailed
	}
	if err := clearLoginBackup(s.Profiles, job.ProfileID, job.ID); err != nil {
		// The old auth is already restored. Keeping a leftover backup is safer
		// than hiding a cleanup failure; reconciliation is idempotent.
		return ErrWriteFailed
	}
	return errorCode
}

func (s *Service) rollbackLogin(job Job, previous []byte, previousExists bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	release, err := authbroker.AcquireProfileWriteLock(ctx, s.Profiles, job.ProfileID)
	if err != nil {
		return err
	}
	defer release()
	if err := restoreAuth(s.Profiles.CodexAuthPath(job.ProfileID), previous, previousExists); err != nil {
		return err
	}
	if job.NewProfile {
		if err := s.removeNewProfile(job.ProfileID); err != nil {
			return err
		}
	}
	return clearLoginBackup(s.Profiles, job.ProfileID, job.ID)
}

func (s *Service) registerNewProfile(profileID string) error {
	registry, err := s.Profiles.Load()
	if err != nil {
		return err
	}
	if _, exists := registry.Find(profile.ProviderCodex, profileID); exists {
		return nil
	}
	if err := registry.Add(profile.Profile{ID: profileID, Provider: profile.ProviderCodex, Isolation: "codex-home"}); err != nil {
		return err
	}
	return s.Profiles.Save(registry)
}

func (s *Service) removeNewProfile(profileID string) error {
	registry, err := s.Profiles.Load()
	if err != nil {
		return err
	}
	if _, exists := registry.Find(profile.ProviderCodex, profileID); !exists {
		return nil
	}
	if err := registry.Remove(profile.ProviderCodex, profileID); err != nil {
		return err
	}
	return s.Profiles.Save(registry)
}

func (s *Service) failLogin(job Job, errorCode string) {
	if current, ok := s.State.Job(job.ID); ok && current.State == JobCanceled {
		return
	}
	_ = s.State.FailJob(job.ID, errorCode)
}

func validVerificationURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil
}

func mapLoginRuntimeError(ctx context.Context, err error) string {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded), errors.Is(err, context.DeadlineExceeded):
		return ErrTimeout
	case errors.Is(ctx.Err(), context.Canceled), errors.Is(err, context.Canceled):
		return ErrCanceled
	case errors.Is(err, codexlogin.ErrDeviceAuthUnavailable):
		return ErrDeviceAuthUnavailable
	default:
		return ErrLoginFailed
	}
}

func (s *Service) Job(id string) (PublicJob, bool) {
	job, ok := s.State.Job(id)
	if !ok {
		return PublicJob{}, false
	}
	return job.Public(), true
}

func (s *Service) CancelJob(id string) (PublicJob, error) {
	job, err := s.State.CancelJob(id)
	if err != nil {
		return PublicJob{}, err
	}
	s.mu.Lock()
	if cancel := s.cancels[id]; cancel != nil {
		cancel()
		delete(s.cancels, id)
	}
	s.mu.Unlock()
	return job.Public(), nil
}

func (s *Service) setCancel(id string, cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancels[id] = cancel
}

func (s *Service) clearCancel(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cancels, id)
}

func mapAuthError(err error) string {
	switch authbroker.Code(err) {
	case authbroker.ErrorNotLoggedIn:
		return ErrAuthFileInvalid
	case authbroker.ErrorReauthRequired:
		return ErrReauthRequired
	default:
		return ErrTemporary
	}
}

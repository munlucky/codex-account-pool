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
	"github.com/munlucky/codex-account-pool/internal/gateway"
	"github.com/munlucky/codex-account-pool/internal/profile"
)

const (
	checkTimeout        = 20 * time.Second
	checkCooldown       = 2 * time.Second
	loginTimeout        = 10 * time.Minute
	workerLeaseDuration = 20 * time.Second
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
	Profiles *profile.Store
	Broker   *authbroker.Broker
	State    *StateStore
	Checker  ProfileChecker
	Now      func() time.Time

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
	response := ProfileListResponse{Diagnostics: s.State.Diagnostics()}
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
	candidate := profile.Profile{ID: profileID, Provider: profile.ProviderCodex, Isolation: "codex-home"}
	if err := profile.ValidateProfile(candidate); err != nil {
		return Job{}, err
	}
	registry, err := s.Profiles.Load()
	if err != nil {
		return Job{}, err
	}
	_, exists := registry.Find(providerName, profileID)
	if job, ok := s.State.ActiveJob(providerName, profileID); ok {
		return Job{}, fmt.Errorf("profile operation already active: %s", job.ID)
	}
	job, err := s.State.CreateJob(JobTypeLogin, providerName, profileID, !exists)
	if err != nil {
		return Job{}, err
	}
	go s.expireLogin(job.ID)
	return job, nil
}

func (s *Service) expireLogin(jobID string) {
	timer := time.NewTimer(loginTimeout)
	defer timer.Stop()
	<-timer.C
	job, ok := s.State.Job(jobID)
	if !ok || (job.State != JobQueued && job.State != JobLoggingIn) {
		return
	}
	_ = s.State.FailJob(jobID, ErrTimeout)
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

func (s *Service) WorkerLease() (Job, bool, error) {
	job, ok, err := s.State.LeaseLogin(workerLeaseDuration)
	if err != nil {
		return Job{}, false, err
	}
	if err := s.markWorkerSeen(); err != nil {
		return Job{}, false, err
	}
	return job, ok, nil
}

func (s *Service) WorkerHeartbeat(id, leaseID string) (PublicJob, error) {
	job, err := s.State.Heartbeat(id, leaseID, workerLeaseDuration)
	if err != nil {
		return PublicJob{}, err
	}
	if err := s.markWorkerSeen(); err != nil {
		return PublicJob{}, err
	}
	return job.Public(), nil
}

func (s *Service) WorkerFinish(id, leaseID string, success bool, errorCode string) (PublicJob, error) {
	job, err := s.State.BeginLoginCompletion(id, leaseID)
	if err != nil {
		return PublicJob{}, err
	}
	if job.Type != JobTypeLogin {
		_ = s.State.FailJob(id, ErrLoginFailed)
		return PublicJob{}, errors.New("job is not a login job")
	}

	if !success {
		if !validWorkerError(errorCode) {
			errorCode = ErrLoginFailed
		}
		finished, err := s.State.FinishLogin(id, leaseID, false, errorCode)
		if err != nil {
			return PublicJob{}, err
		}
		_ = s.markWorkerSeen()
		return finished.Public(), nil
	}

	if _, err := s.Broker.ValidateProfileAuth(job.ProfileID); err != nil {
		finished, finishErr := s.State.FinishLogin(id, leaseID, false, mapAuthError(err))
		if finishErr != nil {
			return PublicJob{}, finishErr
		}
		_ = s.markWorkerSeen()
		return finished.Public(), nil
	}

	registry, err := s.Profiles.Load()
	if err != nil {
		finished, finishErr := s.State.FinishLogin(id, leaseID, false, ErrWriteFailed)
		if finishErr != nil {
			return PublicJob{}, finishErr
		}
		return finished.Public(), nil
	}
	if _, exists := registry.Find(profile.ProviderCodex, job.ProfileID); !exists {
		if !job.NewProfile {
			finished, finishErr := s.State.FinishLogin(id, leaseID, false, ErrWriteFailed)
			if finishErr != nil {
				return PublicJob{}, finishErr
			}
			return finished.Public(), nil
		}
		if err := registry.Add(profile.Profile{ID: job.ProfileID, Provider: profile.ProviderCodex, Isolation: "codex-home"}); err != nil {
			finished, finishErr := s.State.FinishLogin(id, leaseID, false, ErrWriteFailed)
			if finishErr != nil {
				return PublicJob{}, finishErr
			}
			return finished.Public(), nil
		}
		if err := s.Profiles.Save(registry); err != nil {
			finished, finishErr := s.State.FinishLogin(id, leaseID, false, ErrWriteFailed)
			if finishErr != nil {
				return PublicJob{}, finishErr
			}
			return finished.Public(), nil
		}
	}

	go s.finalizeLoginCheck(job, leaseID)
	_ = s.markWorkerSeen()
	return job.Public(), nil
}

func (s *Service) finalizeLoginCheck(job Job, leaseID string) {
	ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
	defer cancel()

	check := CheckResult{Status: StatusTemporarilyUnavailable, ErrorCode: ErrTemporary, CheckedAt: s.now().UTC()}
	if s.Checker != nil {
		check = s.Checker.Check(ctx, job.ProfileID)
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		check = CheckResult{Status: StatusTemporarilyUnavailable, ErrorCode: ErrTimeout, CheckedAt: s.now().UTC()}
	}
	_ = s.State.RecordCheck(profile.ProviderCodex, job.ProfileID, CheckRecord{
		Status: check.Status, ErrorCode: check.ErrorCode, CheckedAt: check.CheckedAt,
	})
	if check.Status == StatusConnected {
		_, _ = s.State.FinishLogin(job.ID, leaseID, true, "")
	} else {
		errorCode := check.ErrorCode
		if errorCode == "" {
			errorCode = ErrTemporary
		}
		_, _ = s.State.FinishLogin(job.ID, leaseID, false, errorCode)
	}
	_ = s.markWorkerSeen()
}

func (s *Service) UpdateWorkerDiagnostics(state string) error {
	state = strings.TrimSpace(state)
	switch state {
	case "configured", "disabled", "unknown":
	default:
		state = "unknown"
	}
	now := s.now().UTC()
	diagnostics := s.State.Diagnostics()
	diagnostics.WorkerSeenAt = &now
	diagnostics.DesktopRoutingState = state
	diagnostics.DesktopCheckedAt = &now
	return s.State.SetDiagnostics(diagnostics)
}

func (s *Service) markWorkerSeen() error {
	now := s.now().UTC()
	diagnostics := s.State.Diagnostics()
	diagnostics.WorkerSeenAt = &now
	if diagnostics.DesktopRoutingState == "" {
		diagnostics.DesktopRoutingState = "unknown"
	}
	return s.State.SetDiagnostics(diagnostics)
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

func validWorkerError(code string) bool {
	switch code {
	case ErrCanceled, ErrTimeout, ErrLoginFailed, ErrAccountMismatch, ErrAuthFileInvalid, ErrWriteFailed, ErrWorkerRestarted:
		return true
	default:
		return false
	}
}

package adminapi

import "time"

const (
	StatusNotLoggedIn             = "not_logged_in"
	StatusUnverified              = "unverified"
	StatusAccessExpiredUnverified = "access_expired_unverified"
	StatusConnected               = "connected"
	StatusReauthRequired          = "reauth_required"
	StatusTemporarilyUnavailable  = "temporarily_unavailable"
	StatusChecking                = "checking"
	StatusLoggingIn               = "logging_in"
)

const (
	JobTypeCheck = "check"
	JobTypeLogin = "login"

	JobQueued     = "queued"
	JobChecking   = "checking"
	JobLoggingIn  = "logging_in"
	JobFinalizing = "finalizing"
	JobSucceeded  = "succeeded"
	JobFailed     = "failed"
	JobCanceled   = "canceled"
)

const (
	ErrNotLoggedIn       = "not_logged_in"
	ErrReauthRequired    = "reauth_required"
	ErrTemporary         = "temporarily_unavailable"
	ErrCanceled          = "canceled"
	ErrTimeout           = "timeout"
	ErrWorkerUnavailable = "worker_unavailable"
	ErrWorkerRestarted   = "worker_restarted"
	ErrServiceRestarted  = "service_restarted"
	ErrLoginFailed       = "login_failed"
	ErrAccountMismatch   = "account_mismatch"
	ErrAuthFileInvalid   = "auth_file_invalid"
	ErrWriteFailed       = "auth_write_failed"
)

type ProfileView struct {
	Provider        string     `json:"provider"`
	ID              string     `json:"id"`
	Active          bool       `json:"active"`
	Status          string     `json:"status"`
	AccessExpiresAt *time.Time `json:"access_expires_at,omitempty"`
	HasRefreshToken bool       `json:"has_refresh_token"`
	LastCheckedAt   *time.Time `json:"last_checked_at,omitempty"`
	ErrorCode       string     `json:"error_code,omitempty"`
	JobID           string     `json:"job_id,omitempty"`
}

type CheckRecord struct {
	Status    string    `json:"status"`
	ErrorCode string    `json:"error_code,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

type Diagnostics struct {
	WorkerSeenAt        *time.Time `json:"worker_seen_at,omitempty"`
	DesktopRoutingState string     `json:"desktop_routing_state"`
	DesktopCheckedAt    *time.Time `json:"desktop_checked_at,omitempty"`
}

type Job struct {
	ID         string    `json:"id"`
	Type       string    `json:"type"`
	Provider   string    `json:"provider"`
	ProfileID  string    `json:"profile_id"`
	NewProfile bool      `json:"new_profile,omitempty"`
	State      string    `json:"state"`
	ErrorCode  string    `json:"error_code,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`

	LeaseID    string    `json:"lease_id,omitempty"`
	LeaseUntil time.Time `json:"lease_until,omitempty"`
}

type PublicJob struct {
	ID         string    `json:"id"`
	Type       string    `json:"type"`
	Provider   string    `json:"provider"`
	ProfileID  string    `json:"profile_id"`
	NewProfile bool      `json:"new_profile,omitempty"`
	State      string    `json:"state"`
	ErrorCode  string    `json:"error_code,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func (j Job) Public() PublicJob {
	state := j.State
	if j.Type == JobTypeLogin && j.State == JobFinalizing {
		state = JobLoggingIn
	}
	return PublicJob{
		ID: j.ID, Type: j.Type, Provider: j.Provider, ProfileID: j.ProfileID,
		NewProfile: j.NewProfile, State: state, ErrorCode: j.ErrorCode,
		CreatedAt: j.CreatedAt, UpdatedAt: j.UpdatedAt,
	}
}

type ProfileListResponse struct {
	Profiles    []ProfileView `json:"profiles"`
	Diagnostics Diagnostics   `json:"diagnostics"`
}

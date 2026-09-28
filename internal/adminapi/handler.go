package adminapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/munlucky/codex-account-pool/internal/adminauth"
)

type Handler struct {
	Service   *Service
	Sessions  *adminauth.Sessions
	WorkerKey string
}

func NewHandler(service *Service, sessions *adminauth.Sessions, workerKey string) *Handler {
	return &Handler{Service: service, Sessions: sessions, WorkerKey: workerKey}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	secureHeaders(w)
	if r.URL.Path == "/admin" || r.URL.Path == "/admin/" {
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, adminHTML)
		return
	}
	if r.URL.Path == "/admin/session" {
		h.handleSession(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/admin/worker/") {
		h.handleWorker(w, r)
		return
	}

	session, ok := h.Sessions.Authenticate(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "admin_auth_required")
		return
	}
	if r.Method != http.MethodGet && !h.Sessions.ValidCSRF(r, session) {
		writeError(w, http.StatusForbidden, "csrf_rejected")
		return
	}

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/admin/profiles":
		response, err := h.Service.ListProfiles()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "admin_state_unavailable")
			return
		}
		writeJSON(w, http.StatusOK, response)
		return
	case strings.HasPrefix(r.URL.Path, "/admin/profiles/"):
		h.handleProfileAction(w, r)
		return
	case strings.HasPrefix(r.URL.Path, "/admin/jobs/"):
		h.handleJob(w, r)
		return
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) handleSession(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		session, ok := h.Sessions.Authenticate(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "admin_auth_required")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"csrf_token": session.CSRFToken,
			"expires_at": session.ExpiresAt,
		})
	case http.MethodPost:
		if !adminauth.SameOrigin(r) {
			writeError(w, http.StatusForbidden, "origin_rejected")
			return
		}
		var input struct {
			Key string `json:"key"`
		}
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		session, err := h.Sessions.Login(input.Key)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid_admin_key")
			return
		}
		adminauth.SetSessionCookie(w, session)
		writeJSON(w, http.StatusOK, map[string]any{
			"csrf_token": session.CSRFToken,
			"expires_at": session.ExpiresAt,
		})
	case http.MethodDelete:
		session, ok := h.Sessions.Authenticate(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "admin_auth_required")
			return
		}
		if !h.Sessions.ValidCSRF(r, session) {
			writeError(w, http.StatusForbidden, "csrf_rejected")
			return
		}
		h.Sessions.Logout(r)
		adminauth.ClearSessionCookie(w)
		w.WriteHeader(http.StatusNoContent)
	default:
		methodNotAllowed(w)
	}
}

func (h *Handler) handleProfileAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/admin/profiles/"), "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
		http.NotFound(w, r)
		return
	}
	providerName, profileID, action := parts[0], parts[1], parts[2]
	var job Job
	var err error
	switch action {
	case JobTypeCheck:
		job, err = h.Service.StartCheck(providerName, profileID)
	case JobTypeLogin:
		job, err = h.Service.StartLogin(providerName, profileID)
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, job.Public())
}

func (h *Handler) handleJob(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/admin/jobs/")
	parts := strings.Split(rest, "/")
	if len(parts) == 1 && r.Method == http.MethodGet {
		job, ok := h.Service.Job(parts[0])
		if !ok {
			writeError(w, http.StatusNotFound, "job_not_found")
			return
		}
		writeJSON(w, http.StatusOK, job)
		return
	}
	if len(parts) == 2 && parts[1] == "cancel" && r.Method == http.MethodPost {
		job, err := h.Service.CancelJob(parts[0])
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, job)
		return
	}
	methodNotAllowed(w)
}

func (h *Handler) handleWorker(w http.ResponseWriter, r *http.Request) {
	if !adminauth.MatchesBearer(r.Header.Get("Authorization"), h.WorkerKey) {
		writeError(w, http.StatusUnauthorized, "worker_auth_required")
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	switch r.URL.Path {
	case "/admin/worker/lease":
		job, ok, err := h.Service.WorkerLease()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "worker_state_unavailable")
			return
		}
		if !ok {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"job_id":      job.ID,
			"provider":    job.Provider,
			"profile_id":  job.ProfileID,
			"new_profile": job.NewProfile,
			"lease_id":    job.LeaseID,
			"lease_until": job.LeaseUntil,
		})
		return
	case "/admin/worker/diagnostics":
		var input struct {
			DesktopRoutingState string `json:"desktop_routing_state"`
		}
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		if err := h.Service.UpdateWorkerDiagnostics(input.DesktopRoutingState); err != nil {
			writeError(w, http.StatusInternalServerError, "worker_state_unavailable")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	rest := strings.TrimPrefix(r.URL.Path, "/admin/worker/jobs/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	switch parts[1] {
	case "status":
		job, ok := h.Service.Job(parts[0])
		if !ok {
			writeError(w, http.StatusNotFound, "job_not_found")
			return
		}
		writeJSON(w, http.StatusOK, job)
	case "heartbeat":
		var input struct {
			LeaseID string `json:"lease_id"`
		}
		if err := decodeJSON(r, &input); err != nil || input.LeaseID == "" {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		job, err := h.Service.WorkerHeartbeat(parts[0], input.LeaseID)
		if err != nil {
			writeError(w, http.StatusConflict, "lease_inactive")
			return
		}
		writeJSON(w, http.StatusOK, job)
	case "complete":
		var input struct {
			LeaseID   string `json:"lease_id"`
			Success   bool   `json:"success"`
			ErrorCode string `json:"error_code,omitempty"`
		}
		if err := decodeJSON(r, &input); err != nil || input.LeaseID == "" {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		job, err := h.Service.WorkerFinish(parts[0], input.LeaseID, input.Success, input.ErrorCode)
		if err != nil {
			writeError(w, http.StatusConflict, "lease_inactive")
			return
		}
		writeJSON(w, http.StatusOK, job)
	default:
		http.NotFound(w, r)
	}
}

func decodeJSON(r *http.Request, out any) error {
	if r == nil || r.Body == nil {
		return errors.New("request body is required")
	}
	decoder := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 32<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("request must contain one JSON value")
	}
	return nil
}

func writeServiceError(w http.ResponseWriter, err error) {
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "not found"):
		writeError(w, http.StatusNotFound, "profile_not_found")
	case strings.Contains(message, "already active"), strings.Contains(message, "rate limited"), strings.Contains(message, "finalization"):
		writeError(w, http.StatusConflict, "operation_conflict")
	default:
		writeError(w, http.StatusBadRequest, "invalid_request")
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code}})
}

func methodNotAllowed(w http.ResponseWriter) {
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
}

func secureHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
}

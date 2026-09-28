package adminapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/munlucky/codex-account-pool/internal/adminauth"
	"github.com/munlucky/codex-account-pool/internal/authbroker"
	"github.com/munlucky/codex-account-pool/internal/profile"
)

func TestAdminHandlerSeparatesAdminClientAndWorkerCredentials(t *testing.T) {
	root := t.TempDir()
	store := profile.NewStore(root)
	broker := authbroker.New(store)
	state, err := OpenStateStore(root)
	if err != nil {
		t.Fatal(err)
	}
	adminKey := "gcr_admin_abcdefghijklmnopqrstuvwxyz0123456789ABCD"
	workerKey := "gcr_worker_abcdefghijklmnopqrstuvwxyz0123456789ABCD"
	sessions := adminauth.NewSessions(adminKey)
	handler := NewHandler(NewService(store, broker, state, nil), sessions, workerKey)

	req := httptest.NewRequest(http.MethodGet, "http://local/admin/profiles", nil)
	req.Header.Set("Authorization", "Bearer gcr_client_not_admin")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("client key status=%d", rr.Code)
	}

	loginBody, _ := json.Marshal(map[string]string{"key": adminKey})
	req = httptest.NewRequest(http.MethodPost, "http://local/admin/session", bytes.NewReader(loginBody))
	req.Header.Set("Origin", "http://local")
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", rr.Code, rr.Body.String())
	}
	var sessionPayload struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &sessionPayload); err != nil {
		t.Fatal(err)
	}
	cookies := rr.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != adminauth.SessionCookieName || !cookies[0].HttpOnly {
		t.Fatalf("cookies=%+v", cookies)
	}

	req = httptest.NewRequest(http.MethodGet, "http://local/admin/profiles", nil)
	req.AddCookie(cookies[0])
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("profiles status=%d body=%s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "http://local/admin/profiles/codex/new-account/login", bytes.NewReader([]byte("{}")))
	req.Header.Set("Origin", "http://local")
	req.AddCookie(cookies[0])
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("missing csrf status=%d", rr.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "http://local/admin/worker/lease", bytes.NewReader([]byte("{}")))
	req.Header.Set("Authorization", "Bearer "+adminKey)
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("admin key used as worker key status=%d", rr.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "http://local/admin/worker/lease", bytes.NewReader([]byte("{}")))
	req.Header.Set("Authorization", "Bearer "+workerKey)
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("worker status=%d body=%s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "http://local/admin/profiles/codex/new-account/login", bytes.NewReader([]byte("{}")))
	req.Header.Set("Origin", "http://local")
	req.Header.Set("X-CSRF-Token", sessionPayload.CSRFToken)
	req.AddCookie(cookies[0])
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("csrf login job status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestAdminSessionRejectsCrossOriginLogin(t *testing.T) {
	root := t.TempDir()
	store := profile.NewStore(root)
	state, err := OpenStateStore(root)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(NewService(store, authbroker.New(store), state, nil), adminauth.NewSessions("gcr_admin_abcdefghijklmnopqrstuvwxyz0123456789ABCD"), "gcr_worker_abcdefghijklmnopqrstuvwxyz0123456789ABCD")
	req := httptest.NewRequest(http.MethodPost, "http://local/admin/session", bytes.NewReader([]byte(`{"key":"gcr_admin_abcdefghijklmnopqrstuvwxyz0123456789ABCD"}`)))
	req.Header.Set("Origin", "http://evil.example")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status=%d", rr.Code)
	}
}

func TestWorkerStatusReturnsBoundedJobWithoutLeaseMaterial(t *testing.T) {
	root := t.TempDir()
	store := profile.NewStore(root)
	state, err := OpenStateStore(root)
	if err != nil {
		t.Fatal(err)
	}
	job, err := state.CreateJob(JobTypeLogin, profile.ProviderCodex, "account-1", false)
	if err != nil {
		t.Fatal(err)
	}
	leased, ok, err := state.LeaseLogin(time.Minute)
	if err != nil || !ok || leased.ID != job.ID {
		t.Fatalf("lease=%+v ok=%v err=%v", leased, ok, err)
	}

	workerKey := "gcr_worker_abcdefghijklmnopqrstuvwxyz0123456789ABCD"
	handler := NewHandler(
		NewService(store, authbroker.New(store), state, nil),
		adminauth.NewSessions("gcr_admin_abcdefghijklmnopqrstuvwxyz0123456789ABCD"),
		workerKey,
	)

	req := httptest.NewRequest(http.MethodPost, "http://local/admin/worker/jobs/"+job.ID+"/status", bytes.NewReader([]byte("{}")))
	req.Header.Set("Authorization", "Bearer "+workerKey)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["id"] != job.ID || payload["state"] != JobLoggingIn {
		t.Fatalf("payload=%+v", payload)
	}
	for _, forbidden := range []string{"lease_id", "lease_until"} {
		if _, exists := payload[forbidden]; exists {
			t.Fatalf("worker status leaked %s: %+v", forbidden, payload)
		}
	}
}

package adminapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/munlucky/codex-account-pool/internal/adminauth"
	"github.com/munlucky/codex-account-pool/internal/authbroker"
	"github.com/munlucky/codex-account-pool/internal/codexlogin"
	"github.com/munlucky/codex-account-pool/internal/profile"
)

type handlerLoginRunner func(context.Context, string, func(codexlogin.Challenge) error) error

func (f handlerLoginRunner) Login(ctx context.Context, home string, cb func(codexlogin.Challenge) error) error {
	return f(ctx, home, cb)
}

func TestAdminHandlerRequiresSessionAndCSRF(t *testing.T) {
	root := t.TempDir()
	store := profile.NewStore(root)
	state, err := OpenStateStore(root)
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store, authbroker.New(store), state, nil)
	service.LoginRunner = handlerLoginRunner(func(context.Context, string, func(codexlogin.Challenge) error) error {
		return codexlogin.ErrDeviceAuthUnavailable
	})
	adminKey := "admin-test-key"
	handler := NewHandler(service, adminauth.NewSessions(adminKey))

	req := httptest.NewRequest(http.MethodGet, "http://local/admin/profiles", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", rr.Code)
	}

	loginBody, _ := json.Marshal(map[string]string{"key": adminKey})
	req = httptest.NewRequest(http.MethodPost, "http://local/admin/session", bytes.NewReader(loginBody))
	req.Header.Set("Origin", "http://local")
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", rr.Code, rr.Body.String())
	}
	var payload struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	cookie := rr.Result().Cookies()[0]

	req = httptest.NewRequest(http.MethodPost, "http://local/admin/profiles/codex/new-account/login", bytes.NewReader([]byte("{}")))
	req.Header.Set("Origin", "http://local")
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("missing csrf status=%d", rr.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "http://local/admin/profiles/codex/new-account/login", bytes.NewReader([]byte("{}")))
	req.Header.Set("Origin", "http://local")
	req.Header.Set("X-CSRF-Token", payload.CSRF)
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("login job status=%d body=%s", rr.Code, rr.Body.String())
	}
	var job PublicJob
	if err := json.Unmarshal(rr.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	_ = waitForTerminalJob(t, service, job.ID)
}

func TestAdminSessionRejectsCrossOriginLogin(t *testing.T) {
	root := t.TempDir()
	store := profile.NewStore(root)
	state, err := OpenStateStore(root)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(NewService(store, authbroker.New(store), state, nil), adminauth.NewSessions("admin-test-key"))
	req := httptest.NewRequest(http.MethodPost, "http://local/admin/session", bytes.NewReader([]byte(`{"key":"admin-test-key"}`)))
	req.Header.Set("Origin", "http://evil.example")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status=%d", rr.Code)
	}
}

func TestRemovedWorkerSurfaceCannotUseBearerAuthentication(t *testing.T) {
	root := t.TempDir()
	store := profile.NewStore(root)
	state, err := OpenStateStore(root)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(NewService(store, authbroker.New(store), state, nil), adminauth.NewSessions("admin-test-key"))
	req := httptest.NewRequest(http.MethodPost, "http://local/admin/worker/lease", bytes.NewReader([]byte("{}")))
	req.Header.Set("Authorization", "Bearer obsolete-worker-key")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("removed worker surface status=%d", rr.Code)
	}
}

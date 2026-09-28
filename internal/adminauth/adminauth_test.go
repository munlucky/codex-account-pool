package adminauth

import (
	"net/http/httptest"
	"testing"
)

func TestAdminAndWorkerKeysAreDistinct(t *testing.T) {
	root := t.TempDir()
	adminKey, err := EnsureAdminKey(root)
	if err != nil {
		t.Fatal(err)
	}
	workerKey, err := EnsureWorkerKey(root)
	if err != nil {
		t.Fatal(err)
	}
	if adminKey == workerKey || !MatchesKey(adminKey, adminKey) || MatchesKey(adminKey, workerKey) {
		t.Fatalf("key separation failed")
	}
	if !MatchesBearer("Bearer "+workerKey, workerKey) || MatchesBearer("Bearer "+adminKey, workerKey) {
		t.Fatalf("bearer separation failed")
	}
}

func TestSessionRequiresSameOriginAndCSRF(t *testing.T) {
	sessions := NewSessions("gcr_admin_test_admin_key_012345678901234567890123456789")
	session, err := sessions.Login("gcr_admin_test_admin_key_012345678901234567890123456789")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "http://127.0.0.1:8317/admin/profiles/codex/one/check", nil)
	req.Host = "127.0.0.1:8317"
	req.Header.Set("Origin", "http://127.0.0.1:8317")
	req.Header.Set("X-CSRF-Token", session.CSRFToken)
	if !sessions.ValidCSRF(req, session) {
		t.Fatal("expected same-origin CSRF request to pass")
	}

	req.Header.Set("Origin", "http://evil.example")
	if sessions.ValidCSRF(req, session) {
		t.Fatal("cross-origin request passed")
	}
	req.Header.Set("Origin", "http://127.0.0.1:8317")
	req.Header.Set("X-CSRF-Token", "wrong")
	if sessions.ValidCSRF(req, session) {
		t.Fatal("wrong CSRF token passed")
	}
}

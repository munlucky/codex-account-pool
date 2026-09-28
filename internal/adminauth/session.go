package adminauth

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const SessionCookieName = "gcr_admin_session"

type Session struct {
	ID        string
	CSRFToken string
	ExpiresAt time.Time
}

type Sessions struct {
	key string

	mu       sync.Mutex
	sessions map[string]Session
	now      func() time.Time
	ttl      time.Duration
}

func NewSessions(adminKey string) *Sessions {
	return &Sessions{
		key: adminKey, sessions: map[string]Session{},
		now: time.Now, ttl: time.Hour,
	}
}

func (s *Sessions) Login(candidate string) (Session, error) {
	if s == nil || !MatchesKey(candidate, s.key) {
		return Session{}, errors.New("invalid administrator key")
	}
	session := Session{
		ID: randomToken("s_"), CSRFToken: randomToken("c_"),
		ExpiresAt: s.now().Add(s.ttl).UTC(),
	}
	s.mu.Lock()
	s.pruneLocked()
	s.sessions[session.ID] = session
	s.mu.Unlock()
	return session, nil
}

func (s *Sessions) Logout(r *http.Request) {
	if s == nil || r == nil {
		return
	}
	cookie, err := r.Cookie(SessionCookieName)
	if err != nil || cookie.Value == "" {
		return
	}
	s.mu.Lock()
	delete(s.sessions, cookie.Value)
	s.mu.Unlock()
}

func (s *Sessions) Authenticate(r *http.Request) (Session, bool) {
	if s == nil || r == nil {
		return Session{}, false
	}
	cookie, err := r.Cookie(SessionCookieName)
	if err != nil || cookie.Value == "" {
		return Session{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	session, ok := s.sessions[cookie.Value]
	return session, ok
}

func (s *Sessions) ValidCSRF(r *http.Request, session Session) bool {
	if r == nil || session.CSRFToken == "" {
		return false
	}
	return MatchesKey(r.Header.Get("X-CSRF-Token"), session.CSRFToken) && SameOrigin(r)
}

func SameOrigin(r *http.Request) bool {
	if r == nil {
		return false
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return false
	}
	parsed, err := url.Parse(origin)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return false
	}
	return strings.EqualFold(parsed.Host, r.Host)
}

func SetSessionCookie(w http.ResponseWriter, session Session) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookieName, Value: session.ID, Path: "/admin",
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Expires: session.ExpiresAt,
	})
}

func ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookieName, Value: "", Path: "/admin",
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		MaxAge: -1, Expires: time.Unix(1, 0),
	})
}

func (s *Sessions) pruneLocked() {
	now := s.now()
	for id, session := range s.sessions {
		if !session.ExpiresAt.After(now) {
			delete(s.sessions, id)
		}
	}
}

func randomToken(prefix string) string {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(raw)
}

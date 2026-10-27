package main

import (
	"crypto/subtle"
	"net/http"
	"sync"
	"time"
)

const sessionCookie = "puppetron_session"

// sessionStore is a simple in-memory session table.
type sessionStore struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]time.Time
}

func newSessionStore(ttl time.Duration) *sessionStore {
	return &sessionStore{ttl: ttl, m: map[string]time.Time{}}
}

func (s *sessionStore) create() string {
	id := randomHex(32)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gc()
	s.m[id] = time.Now().Add(s.ttl)
	return id
}

func (s *sessionStore) valid(id string) bool {
	if id == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.m[id]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(s.m, id)
		return false
	}
	// sliding expiry
	s.m[id] = time.Now().Add(s.ttl)
	return true
}

func (s *sessionStore) revoke(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, id)
}

func (s *sessionStore) gc() {
	now := time.Now()
	for k, exp := range s.m {
		if now.After(exp) {
			delete(s.m, k)
		}
	}
}

func (s *Server) checkCredentials(user, pass string) bool {
	u := subtle.ConstantTimeCompare([]byte(user), []byte(s.cfg.User))
	p := subtle.ConstantTimeCompare([]byte(pass), []byte(s.cfg.Password))
	return u == 1 && p == 1
}

func (s *Server) isTLS(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, id string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.isTLS(r),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   maxAge,
	})
}

func (s *Server) authenticated(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	return s.sessions.valid(c.Value)
}

// requireAPIAuth guards JSON endpoints: 401 when not logged in, 403 when a
// state-changing request lacks the anti-CSRF header.
func (s *Server) requireAPIAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authenticated(r) {
			writeError(w, http.StatusUnauthorized, "not authenticated")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-Requested-With") != "puppetron" {
			writeError(w, http.StatusForbidden, "missing X-Requested-With header")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requirePageAuth guards HTML pages and static noVNC files: redirect to /login.
func (s *Server) requirePageAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authenticated(r) {
			http.Redirect(w, r, "/login?next="+r.URL.RequestURI(), http.StatusFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

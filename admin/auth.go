package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

const (
	sessionCookie = "vp_admin"
	sessionTTL    = 12 * time.Hour
	maxFailures   = 5
	lockout       = 15 * time.Minute
)

type auth struct {
	pwHash [32]byte

	mu       sync.Mutex
	sessions map[string]time.Time   // session token -> expiry
	failures map[string][]time.Time // client IP -> recent failed logins
}

func newAuth(password string) *auth {
	return &auth{pwHash: sha256.Sum256([]byte(password)), sessions: map[string]time.Time{}, failures: map[string][]time.Time{}}
}

func (a *auth) valid(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil || len(c.Value) != 64 {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.sessions[c.Value]
	if !ok || time.Now().After(exp) {
		delete(a.sessions, c.Value)
		return false
	}
	return true
}

// require wraps h: pages redirect to the login screen, API calls get 401.
func (a *auth) require(h http.Handler, page bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.valid(r) {
			if page {
				http.Redirect(w, r, "/login.html", http.StatusSeeOther)
				return
			}
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "signed out"})
			return
		}
		h.ServeHTTP(w, r)
	})
}

func (a *auth) locked(ip string, now time.Time) bool {
	recent := a.failures[ip][:0]
	for _, t := range a.failures[ip] {
		if now.Sub(t) < lockout {
			recent = append(recent, t)
		}
	}
	a.failures[ip] = recent
	return len(recent) >= maxFailures
}

func (a *auth) login(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	now := time.Now()
	a.mu.Lock()
	if a.locked(ip, now) {
		a.mu.Unlock()
		http.Redirect(w, r, "/login.html?e=locked", http.StatusSeeOther)
		return
	}
	a.mu.Unlock()

	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	got := sha256.Sum256([]byte(r.FormValue("password")))
	if subtle.ConstantTimeCompare(got[:], a.pwHash[:]) != 1 {
		a.mu.Lock()
		a.failures[ip] = append(a.failures[ip], now)
		a.mu.Unlock()
		time.Sleep(500 * time.Millisecond)
		http.Redirect(w, r, "/login.html?e=wrong", http.StatusSeeOther)
		return
	}

	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		http.Error(w, "error", http.StatusInternalServerError)
		return
	}
	token := hex.EncodeToString(b)
	a.mu.Lock()
	delete(a.failures, ip)
	for k, exp := range a.sessions {
		if now.After(exp) {
			delete(a.sessions, k)
		}
	}
	a.sessions[token] = now.Add(sessionTTL)
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: int(sessionTTL.Seconds())})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *auth) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		a.mu.Lock()
		delete(a.sessions, c.Value)
		a.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	http.Redirect(w, r, "/login.html", http.StatusSeeOther)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

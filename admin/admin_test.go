package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestCheckPrivate(t *testing.T) {
	for addr, ok := range map[string]bool{
		"100.89.127.14:2530": true,
		"127.0.0.1:2530":     true,
		"192.168.1.5:2530":   true,
		"0.0.0.0:2530":       false,
		":2530":              false,
		"8.8.8.8:2530":       false,
	} {
		if err := checkPrivate(addr); (err == nil) != ok {
			t.Errorf("%s: got err=%v, want ok=%v", addr, err, ok)
		}
	}
}

func loginReq(pw string) *http.Request {
	r := httptest.NewRequest("POST", "/login", strings.NewReader(url.Values{"password": {pw}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.RemoteAddr = "100.64.0.9:5555"
	return r
}

func TestLoginSessionAndLockout(t *testing.T) {
	a := newAuth("correct horse battery")
	protected := a.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }), false)

	// Signed out: API calls get 401.
	rec := httptest.NewRecorder()
	protected.ServeHTTP(rec, httptest.NewRequest("GET", "/api/overview", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("signed out: %d", rec.Code)
	}

	// Right password → session cookie that unlocks the API.
	rec = httptest.NewRecorder()
	a.login(rec, loginReq("correct horse battery"))
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != sessionCookie || !cookies[0].HttpOnly {
		t.Fatalf("no session cookie: %+v", cookies)
	}
	req := httptest.NewRequest("GET", "/api/overview", nil)
	req.AddCookie(cookies[0])
	rec = httptest.NewRecorder()
	protected.ServeHTTP(rec, req)
	if rec.Code != 204 {
		t.Fatalf("with session: %d", rec.Code)
	}

	// Five wrong passwords lock the IP out, even for the right one.
	for i := 0; i < maxFailures; i++ {
		rec = httptest.NewRecorder()
		a.login(rec, loginReq("wrong"))
		if loc := rec.Header().Get("Location"); loc != "/login.html?e=wrong" {
			t.Fatalf("attempt %d: %s", i, loc)
		}
	}
	rec = httptest.NewRecorder()
	a.login(rec, loginReq("correct horse battery"))
	if loc := rec.Header().Get("Location"); loc != "/login.html?e=locked" {
		t.Fatalf("not locked out: %s", loc)
	}
}

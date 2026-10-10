// voxelport-admin is the operator dashboard for a VoxelPort home server.
//
// It listens on a private address only (the Tailscale IP, so it is never on
// the public internet), asks for a password, and shows: live tunnels and
// players from the relay's operator API, website visits from voxelport-web,
// the machine's CPU / RAM / disk / network, service health, and GitHub (and
// optionally Microsoft Store) downloads. It can also kick or ban a tunnel,
// hide a server-list entry and release a custom address, by calling the
// relay's loopback operator API.
//
// Configuration (EnvironmentFile=/etc/voxelport/admin.env):
//
//	VOXELPORT_ADMIN_TOKEN     shared with the relay and website (24+ chars)
//	VOXELPORT_ADMIN_PASSWORD  the dashboard password (12+ chars)
//	STORE_TENANT_ID, STORE_CLIENT_ID, STORE_CLIENT_SECRET   optional: Microsoft
//	                          Store acquisitions via the Partner Center analytics API
package main

import (
	"embed"
	"errors"
	"flag"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"time"
)

//go:embed web
var webFS embed.FS

func main() {
	listen := flag.String("listen", "100.89.127.14:2530", "private address to serve the dashboard on (the Tailscale IP)")
	relayAPI := flag.String("relay", "http://127.0.0.1:2528", "relay operator API")
	webAPI := flag.String("web", "http://127.0.0.1:2529", "website stats API")
	iface := flag.String("iface", "", "network interface to chart (default: the busiest physical one)")
	repo := flag.String("github-repo", "VOXELPORT/VoxelPort-App", "GitHub repo whose release downloads are counted")
	storeApp := flag.String("store-app", "9NGRX9CFNBD6", "Microsoft Store app ID")
	flag.Parse()

	token := os.Getenv("VOXELPORT_ADMIN_TOKEN")
	password := os.Getenv("VOXELPORT_ADMIN_PASSWORD")
	if len(token) < 24 {
		log.Fatal("VOXELPORT_ADMIN_TOKEN must be set (24+ characters)")
	}
	if len(password) < 12 {
		log.Fatal("VOXELPORT_ADMIN_PASSWORD must be set (12+ characters)")
	}
	if err := checkPrivate(*listen); err != nil {
		log.Fatal(err)
	}

	up := &upstreams{relay: *relayAPI, web: *webAPI, token: token, client: &http.Client{Timeout: 4 * time.Second}}
	sys := newSampler(*iface, up)
	go sys.run()
	dl := newDownloads(*repo, *storeApp, os.Getenv("STORE_TENANT_ID"), os.Getenv("STORE_CLIENT_ID"), os.Getenv("STORE_CLIENT_SECRET"))
	go dl.run()

	auth := newAuth(password)
	static, _ := fs.Sub(webFS, "web")
	files := http.FileServer(http.FS(static))

	mux := http.NewServeMux()
	mux.HandleFunc("POST /login", auth.login)
	mux.HandleFunc("POST /logout", auth.logout)
	mux.Handle("GET /login.html", files)
	mux.Handle("GET /app.css", files)
	mux.Handle("GET /login.js", files)
	mux.Handle("GET /", auth.require(files, true))
	mux.Handle("GET /api/overview", auth.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, overview(up, sys, dl))
	}), false))
	mux.Handle("GET /api/history", auth.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, sys.history(r.URL.Query().Get("range")))
	}), false))
	mux.Handle("POST /api/action", auth.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only the dashboard's own script sends this header; a cross-site form can't.
		if r.Header.Get("X-VP-Admin") != "1" {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
			return
		}
		body, err := readSmall(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
			return
		}
		code, out := up.action(body)
		writeJSON(w, code, out)
	}), false))

	srv := &http.Server{
		Addr:              *listen,
		Handler:           securityHeaders(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Printf("VoxelPort admin on http://%s", *listen)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// checkPrivate refuses to listen on a public or wildcard address.
func checkPrivate(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsUnspecified() {
		return errors.New("-listen must be a specific private IP (the Tailscale address), not a wildcard")
	}
	cgnat := &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}
	if !(ip.IsLoopback() || ip.IsPrivate() || cgnat.Contains(ip)) {
		return errors.New("-listen must be a private address (Tailscale 100.x, LAN or loopback)")
	}
	return nil
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; form-action 'self'; base-uri 'none'")
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("Cache-Control", "no-store")
		h.ServeHTTP(w, r)
	})
}

func clientIP(r *http.Request) string {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

func readSmall(r *http.Request) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r.Body, 4097))
	if err != nil || len(b) > 4096 {
		return nil, errors.New("bad body")
	}
	return b, nil
}

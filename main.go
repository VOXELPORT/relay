package main

import (
	"context"
	"crypto/tls"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed panel.html
var panelHTML string

// shutdownTimeout bounds how long graceful shutdown may take before the
// process force-exits, so a stuck connection can't prevent systemd (or any
// other supervisor) from being able to stop the service.
const shutdownTimeout = 15 * time.Second

func main() {
	listen := flag.String("listen", ":8080", "panel + WebSocket listen address")
	minPort := flag.Int("min-port", 25500, "lowest public port assignable to a tunnel")
	maxPort := flag.Int("max-port", 25999, "highest public port assignable to a tunnel")
	cert := flag.String("tls-cert", os.Getenv("RELAY_CERT"), "TLS certificate file (PEM) — enables wss://")
	key := flag.String("tls-key", os.Getenv("RELAY_KEY"), "TLS key file (PEM)")
	secret := flag.String("token-secret", os.Getenv("RELAY_TOKEN_SECRET"), "if set, only accept host tokens shaped vp_<secret>… (gates a public relay)")
	maxTunnelsPerIP := flag.Int("max-tunnels-per-ip", 5, "maximum simultaneous active tunnels from one source IP")
	maxPlayersPerIP := flag.Int("max-players-per-ip", 16, "maximum simultaneous player connections from one source IP, per tunnel")
	trustedProxyCIDRs := flag.String("trusted-proxy-cidrs", "", "comma-separated CIDRs allowed to supply X-Forwarded-For (loopback is always trusted; production Caddy runs on the same host)")
	directListen := flag.String("direct-listen", "", "optional second listener serving wss:// directly (e.g. :26499), bypassing any CDN/tunnel in front of -listen")
	directCert := flag.String("direct-tls-cert", "", "TLS certificate (PEM) for -direct-listen; reloaded from disk when it changes")
	directKey := flag.String("direct-tls-key", "", "TLS key (PEM) for -direct-listen")
	publicHost := flag.String("public-host", "play.voxelport.in", "hostname players use with a port; also the target of custom-address DNS records")
	dataDir := flag.String("data-dir", "", "directory for persistent state (custom addresses, server-list takedowns); empty disables custom addresses")
	dnsZone := flag.String("dns-zone", "voxelport.in", "Cloudflare zone custom addresses are created in (token from $CF_DNS_API_TOKEN)")
	adminAddr := flag.String("admin-addr", "", "loopback address for the operator API used by voxelport-admin (e.g. 127.0.0.1:2528; token from $VOXELPORT_ADMIN_TOKEN)")
	flag.Parse()

	if err := validateConfig(*minPort, *maxPort, *maxTunnelsPerIP, *maxPlayersPerIP); err != nil {
		log.Fatalf("invalid configuration: %v", err)
	}
	if err := validateDirectListener(*directListen, *directCert, *directKey, *minPort, *maxPort); err != nil {
		log.Fatalf("invalid configuration: %v", err)
	}
	trustedProxies, err := parseTrustedProxies(*trustedProxyCIDRs)
	if err != nil {
		log.Fatalf("invalid -trusted-proxy-cidrs: %v", err)
	}

	var names *nameStore
	var hidden *hiddenList
	var supporters *supporterList
	if *dataDir != "" {
		hidden = newHiddenList(filepath.Join(*dataDir, "hidden.txt"))
		supporters = newHiddenList(filepath.Join(*dataDir, "supporters.txt"))
		if token := os.Getenv("CF_DNS_API_TOKEN"); token != "" {
			names, err = loadNameStore(filepath.Join(*dataDir, "names.json"), *dnsZone, newCloudflareDNS(token, *dnsZone, *publicHost))
			if err != nil {
				log.Fatalf("custom addresses: %v", err)
			}
			log.Printf("custom addresses ENABLED under *.%s", *dnsZone)
		} else {
			log.Printf("custom addresses disabled: CF_DNS_API_TOKEN is not set")
		}
	}

	r := newRelay(relayConfig{
		MinPort:               *minPort,
		MaxPort:               *maxPort,
		TokenSecret:           *secret,
		MaxTunnelsPerIP:       *maxTunnelsPerIP,
		MaxPlayersPerIPTunnel: *maxPlayersPerIP,
		TrustedProxies:        trustedProxies,
		Names:                 names,
		PublicHost:            *publicHost,
		Hidden:                hidden,
		Supporters:            supporters,
	})
	if names != nil {
		go r.nameSweepLoop()
		r.startDirectCleanup()
	}
	r.startAdmin(*adminAddr, os.Getenv("VOXELPORT_ADMIN_TOKEN"), *dataDir)
	if *secret != "" {
		log.Printf("token gate ENABLED — only vp_<secret>… tokens accepted")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", r.servePanel)
	mux.HandleFunc("/ws", r.handleWS)
	mux.HandleFunc("/api/status", r.apiStatus)
	mux.HandleFunc("/api/servers", r.apiServers)

	srv := &http.Server{
		Addr:    *listen,
		Handler: mux,
		// Bounds only the time to read request headers, not the connection's
		// overall lifetime — safe for the long-lived /ws upgrade connections.
		// A blanket ReadTimeout/WriteTimeout would kill every active tunnel
		// the instant it elapsed, so those are deliberately left unset.
		ReadHeaderTimeout: 10 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1MB — Go's own default, set explicitly for clarity
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 2)

	// Optional direct wss:// listener. Same handlers as the main one, but its
	// own TLS — so hosts can skip a CDN/tunnel hop that may route them far
	// away (e.g. Cloudflare sending Indian ISPs via Singapore).
	var directSrv *http.Server
	if *directListen != "" {
		rc, err := newReloadingCert(*directCert, *directKey)
		if err != nil {
			log.Fatalf("direct listener: %v", err)
		}
		directSrv = &http.Server{
			Addr:              *directListen,
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second,
			MaxHeaderBytes:    1 << 20,
			TLSConfig: &tls.Config{
				MinVersion:     tls.VersionTLS12,
				GetCertificate: rc.GetCertificate,
			},
		}
		go func() {
			log.Printf("VoxelPort relay direct wss:// listener on %s", *directListen)
			serveErr <- directSrv.ListenAndServeTLS("", "")
		}()
	}

	go func() {
		log.Printf("VoxelPort relay listening on %s, ports %d-%d (max %d tunnels/IP, %d players/IP/tunnel)",
			*listen, *minPort, *maxPort, *maxTunnelsPerIP, *maxPlayersPerIP)
		if *cert != "" {
			serveErr <- srv.ListenAndServeTLS(*cert, *key)
		} else {
			serveErr <- srv.ListenAndServe()
		}
	}()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("relay stopped: %v", err)
		}
	case <-ctx.Done():
		log.Printf("shutdown signal received, draining…")
		// Close every active tunnel first — this unblocks each handleWS
		// goroutine's ws.ReadMessage() call, so the HTTP server's own
		// Shutdown (which waits for in-flight handlers to return) completes
		// promptly instead of waiting for connections that would otherwise
		// live for the rest of the process's lifetime.
		r.shutdown()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if directSrv != nil {
			go directSrv.Shutdown(shutdownCtx)
		}
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("forced shutdown after %s: %v", shutdownTimeout, err)
		} else {
			log.Printf("shutdown complete")
		}
	}
}

// validateDirectListener checks the optional direct listener flags: cert and
// key are required with it, and its port must not overlap the tunnel range
// (the relay would otherwise race itself for that port).
func validateDirectListener(addr, cert, key string, minPort, maxPort int) error {
	if addr == "" {
		if cert != "" || key != "" {
			return errors.New("-direct-tls-cert/-direct-tls-key require -direct-listen")
		}
		return nil
	}
	if cert == "" || key == "" {
		return errors.New("-direct-listen requires -direct-tls-cert and -direct-tls-key")
	}
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("-direct-listen: %w", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return errors.New("-direct-listen port must be between 1 and 65535")
	}
	if port >= minPort && port <= maxPort {
		return fmt.Errorf("-direct-listen port %d overlaps the tunnel port range %d-%d", port, minPort, maxPort)
	}
	return nil
}

// validateConfig rejects startup configuration that could panic, loop
// incorrectly, or expose unexpected ports, instead of failing confusingly
// (or dangerously) later at runtime.
func validateConfig(minPort, maxPort, maxTunnelsPerIP, maxPlayersPerIP int) error {
	if minPort < 1 || minPort > 65535 {
		return errors.New("-min-port must be between 1 and 65535")
	}
	if maxPort < 1 || maxPort > 65535 {
		return errors.New("-max-port must be between 1 and 65535")
	}
	if minPort > maxPort {
		return errors.New("-min-port must not be greater than -max-port")
	}
	if maxTunnelsPerIP < 1 {
		return errors.New("-max-tunnels-per-ip must be at least 1")
	}
	if maxPlayersPerIP < 1 {
		return errors.New("-max-players-per-ip must be at least 1")
	}
	return nil
}

func parseTrustedProxies(csv string) ([]*net.IPNet, error) {
	csv = strings.TrimSpace(csv)
	if csv == "" {
		return nil, nil
	}
	var nets []*net.IPNet
	for _, part := range strings.Split(csv, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		_, n, err := net.ParseCIDR(part)
		if err != nil {
			return nil, err
		}
		nets = append(nets, n)
	}
	return nets, nil
}

func (r *relay) servePanel(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/" {
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(panelHTML))
}

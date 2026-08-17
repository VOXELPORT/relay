package main

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
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
	flag.Parse()

	if err := validateConfig(*minPort, *maxPort, *maxTunnelsPerIP, *maxPlayersPerIP); err != nil {
		log.Fatalf("invalid configuration: %v", err)
	}
	trustedProxies, err := parseTrustedProxies(*trustedProxyCIDRs)
	if err != nil {
		log.Fatalf("invalid -trusted-proxy-cidrs: %v", err)
	}

	r := newRelay(relayConfig{
		MinPort:               *minPort,
		MaxPort:               *maxPort,
		TokenSecret:           *secret,
		MaxTunnelsPerIP:       *maxTunnelsPerIP,
		MaxPlayersPerIPTunnel: *maxPlayersPerIP,
		TrustedProxies:        trustedProxies,
	})
	if *secret != "" {
		log.Printf("token gate ENABLED — only vp_<secret>… tokens accepted")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", r.servePanel)
	mux.HandleFunc("/ws", r.handleWS)
	mux.HandleFunc("/api/status", r.apiStatus)

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

	serveErr := make(chan error, 1)
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
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("forced shutdown after %s: %v", shutdownTimeout, err)
		} else {
			log.Printf("shutdown complete")
		}
	}
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

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
	maxPort := flag.Int("max-port", 29999, "highest public port assignable to a tunnel")
	reservedPorts := flag.String("reserved-ports", "25565", "comma-separated ports inside the range that are never handed to a tunnel (ports other programs on this machine use; 25565 is Minecraft's own default)")
	namePortGrace := flag.Duration("name-port-grace", defaultNamePortGrace, "how long a custom address keeps its port reserved after its host was last seen (supporters' and granted addresses keep it for good)")
	maxReservedFraction := flag.Float64("max-reserved-port-fraction", defaultMaxReservedFraction, "largest share of the port range that custom addresses may reserve (0-1]; new claims fail once it is reached")
	nameClaimsPerIPDay := flag.Int("name-claims-per-ip-day", defaultNameClaimsPerIPDay, "new custom addresses one IPv4 address may claim per 24 hours")
	nameClaimsPerIP6Day := flag.Int("name-claims-per-ip6-day", defaultNameClaimsPerIPDay6, "new custom addresses one IPv6 /64 may claim per 24 hours")
	cert := flag.String("tls-cert", os.Getenv("RELAY_CERT"), "TLS certificate file (PEM) — enables wss://")
	key := flag.String("tls-key", os.Getenv("RELAY_KEY"), "TLS key file (PEM)")
	secret := flag.String("token-secret", os.Getenv("RELAY_TOKEN_SECRET"), "if set, only accept host tokens shaped vp_<secret>… (gates a public relay)")
	maxTunnelsPerIP := flag.Int("max-tunnels-per-ip", defaultMaxTunnelsPerIP, "maximum simultaneous active tunnels from one IPv4 address (many people can share one: mobile networks)")
	maxTunnelsPerIP6 := flag.Int("max-tunnels-per-ip6", defaultMaxTunnelsPerIP6, "maximum simultaneous active tunnels from one IPv6 /64")
	maxPlayersPerIP := flag.Int("max-players-per-ip", defaultMaxPlayersPerIPTunnel, "maximum simultaneous player connections from one IPv4 address, per tunnel")
	maxPlayersPerIP6 := flag.Int("max-players-per-ip6", defaultMaxPlayersPerIPTunnel6, "maximum simultaneous player connections from one IPv6 /64, per tunnel")
	regLimitIP := flag.Int("reg-limit-ip", regLimitMax, "registration attempts per minute from one IPv4 address")
	regLimitIP6 := flag.Int("reg-limit-ip6", regLimitMax6, "registration attempts per minute from one IPv6 /64")
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
	for name, v := range map[string]int{"-max-tunnels-per-ip6": *maxTunnelsPerIP6, "-max-players-per-ip6": *maxPlayersPerIP6, "-reg-limit-ip": *regLimitIP, "-reg-limit-ip6": *regLimitIP6} {
		if v < 1 {
			log.Fatalf("invalid configuration: %s must be at least 1", name)
		}
	}
	reserved, err := parsePortList(*reservedPorts)
	if err != nil {
		log.Fatalf("invalid -reserved-ports: %v", err)
	}
	if err := validateDirectListener(*directListen, *directCert, *directKey, *minPort, *maxPort, reserved); err != nil {
		log.Fatalf("invalid configuration: %v", err)
	}
	if err := validateCapacity(*minPort, *maxPort, reserved, *listen, *namePortGrace, *maxReservedFraction, *nameClaimsPerIPDay, *nameClaimsPerIP6Day); err != nil {
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

		MaxTunnelsPerIP6:       *maxTunnelsPerIP6,
		MaxPlayersPerIPTunnel6: *maxPlayersPerIP6,
		RegLimitIP:             *regLimitIP,
		RegLimitIP6:            *regLimitIP6,
		TrustedProxies:         trustedProxies,
		Names:                  names,
		PublicHost:             *publicHost,
		Hidden:                 hidden,
		Supporters:             supporters,

		NamePortGrace:           *namePortGrace,
		MaxReservedPortFraction: *maxReservedFraction,
		NameClaimsPerIPDay:      *nameClaimsPerIPDay,
		NameClaimsPerIPDay6:     *nameClaimsPerIP6Day,
		ReservedPorts:           reserved,
	})
	if names != nil {
		go r.nameSweepLoop()
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
		log.Printf("VoxelPort relay listening on %s, ports %d-%d (per IPv4: %d tunnels, %d players/tunnel, %d registrations/min; per IPv6 /64: %d, %d, %d)",
			*listen, *minPort, *maxPort, *maxTunnelsPerIP, *maxPlayersPerIP, *regLimitIP, *maxTunnelsPerIP6, *maxPlayersPerIP6, *regLimitIP6)
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
func validateDirectListener(addr, cert, key string, minPort, maxPort int, reserved []int) error {
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
	if port >= minPort && port <= maxPort && !containsPort(reserved, port) {
		return fmt.Errorf("-direct-listen port %d overlaps the tunnel port range %d-%d (add %d to -reserved-ports to keep it out of the pool, or move the range)", port, minPort, maxPort, port)
	}
	return nil
}

func containsPort(ports []int, port int) bool {
	for _, p := range ports {
		if p == port {
			return true
		}
	}
	return false
}

// parsePortList reads "25565,26499" into ports (an empty string is no ports).
func parsePortList(csv string) ([]int, error) {
	var out []int
	for _, part := range strings.Split(csv, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		p, err := strconv.Atoi(part)
		if err != nil || p < 1 || p > 65535 {
			return nil, fmt.Errorf("%q is not a port between 1 and 65535", part)
		}
		out = append(out, p)
	}
	return out, nil
}

// validateCapacity checks the settings that decide how many hosts and custom addresses the relay can hold: that the
// control listener's port is not one tunnels could be given, that something is left of the range once the reserved
// ports are taken out, and that the custom-address rules make sense.
func validateCapacity(minPort, maxPort int, reserved []int, listenAddr string, grace time.Duration, fraction float64, claims4, claims6 int) error {
	if _, portStr, err := net.SplitHostPort(listenAddr); err == nil {
		if port, err := strconv.Atoi(portStr); err == nil && port >= minPort && port <= maxPort && !containsPort(reserved, port) {
			return fmt.Errorf("-listen port %d overlaps the tunnel port range %d-%d (add %d to -reserved-ports to keep it out of the pool, or move the range)", port, minPort, maxPort, port)
		}
	}
	inRange := 0
	for p := minPort; p <= maxPort; p++ {
		if !containsPort(reserved, p) {
			inRange++
		}
	}
	if inRange == 0 {
		return errors.New("every port in -min-port..-max-port is listed in -reserved-ports, so no tunnel could get one")
	}
	if grace <= 0 {
		return errors.New("-name-port-grace must be longer than zero (use a long duration such as 1440h to keep ports for a name's whole life)")
	}
	if fraction <= 0 || fraction > 1 {
		return errors.New("-max-reserved-port-fraction must be above 0 and at most 1")
	}
	if claims4 < 1 || claims6 < 1 {
		return errors.New("-name-claims-per-ip-day and -name-claims-per-ip6-day must be at least 1")
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

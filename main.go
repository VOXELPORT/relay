package main

import (
	_ "embed"
	"flag"
	"log"
	"net/http"
	"os"
)

//go:embed panel.html
var panelHTML string

func main() {
	listen := flag.String("listen", ":8080", "panel + WebSocket listen address")
	minPort := flag.Int("min-port", 25500, "lowest public port assignable to a tunnel")
	maxPort := flag.Int("max-port", 25999, "highest public port assignable to a tunnel")
	cert := flag.String("tls-cert", os.Getenv("RELAY_CERT"), "TLS certificate file (PEM) — enables wss://")
	key := flag.String("tls-key", os.Getenv("RELAY_KEY"), "TLS key file (PEM)")
	secret := flag.String("token-secret", os.Getenv("RELAY_TOKEN_SECRET"), "if set, only accept host tokens shaped vp_<secret>… (gates a public relay)")
	flag.Parse()

	r := newRelay(*minPort, *maxPort, *secret)
	if *secret != "" {
		log.Printf("token gate ENABLED — only vp_<secret>… tokens accepted")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", r.servePanel)
	mux.HandleFunc("/ws", r.handleWS)
	mux.HandleFunc("/api/status", r.apiStatus)

	srv := &http.Server{Addr: *listen, Handler: mux}

	if *cert != "" {
		log.Printf("VoxelPort relay listening on %s (TLS), ports %d-%d", *listen, *minPort, *maxPort)
		log.Fatal(srv.ListenAndServeTLS(*cert, *key))
	} else {
		log.Printf("VoxelPort relay listening on %s (plain HTTP), ports %d-%d", *listen, *minPort, *maxPort)
		log.Fatal(srv.ListenAndServe())
	}
}

func (r *relay) servePanel(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/" {
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(panelHTML))
}

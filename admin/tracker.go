package main

import (
	"encoding/json"
	"strings"
	"time"
)

// relayState is the part of the relay's /state the tracker needs.
type relayState struct {
	PublicHost string `json:"public_host"`
	Tunnels    []struct {
		ID          string    `json:"id"`
		Port        int       `json:"port"`
		Name        string    `json:"name"`
		Since       time.Time `json:"since"`
		Players     []string  `json:"players"`
		UDP         bool      `json:"udp"`
		UDPSessions int       `json:"udp_sessions"`
		BytesIn     int64     `json:"bytes_in"`
		BytesOut    int64     `json:"bytes_out"`
		Conns       int64     `json:"conns"`
		Peak        int       `json:"peak_players"`
		Listing     *struct {
			Title   string `json:"title"`
			Mode    string `json:"mode"`
			Version string `json:"version"`
		} `json:"listing"`
	} `json:"tunnels"`
	Names []struct {
		Name string `json:"name"`
		ID   string `json:"id"`
		Port int    `json:"port"`
	} `json:"names"`
}

// liveSession accumulates one tunnel's life. Player IPs are held in memory
// only, to count distinct players; they are never written to disk.
type liveSession struct {
	ID, Name, Title, Mode, Version string
	Port                           int
	Since                          time.Time
	Bedrock, Listed                bool
	Peak, PeakBedrock              int
	players                        map[string]bool
	BytesIn, BytesOut, Conns       int64
}

// tracker turns successive relay snapshots into session records and events.
type tracker struct {
	st       *store
	started  bool
	sessions map[string]*liveSession
	names    map[string]string // name -> id
	services map[string]string
	relayOK  bool
	webOK    bool
}

func newTracker(st *store) *tracker {
	return &tracker{st: st, sessions: map[string]*liveSession{}, names: map[string]string{}}
}

func (tr *tracker) observe(relay map[string]any, relayErr error, webErr error, services map[string]string) {
	first := !tr.started
	tr.started = true

	// Upstream reachability and service health changes.
	if relayOK := relayErr == nil; first || relayOK != tr.relayOK {
		if !first {
			tr.st.event(map[bool]string{true: "relay_api_back", false: "relay_api_down"}[relayOK], map[string]any{"error": errText(relayErr)})
		}
		tr.relayOK = relayOK
	}
	if webOK := webErr == nil; first || webOK != tr.webOK {
		if !first {
			tr.st.event(map[bool]string{true: "web_stats_back", false: "web_stats_down"}[webOK], map[string]any{"error": errText(webErr)})
		}
		tr.webOK = webOK
	}
	if tr.services != nil {
		for name, state := range services {
			if prev := tr.services[name]; prev != "" && prev != state {
				tr.st.event("service", map[string]any{"service": name, "from": prev, "to": state})
			}
		}
	}
	tr.services = services

	if relay == nil {
		return // can't tell who's online; keep sessions open until the relay answers again
	}
	raw, _ := json.Marshal(relay)
	var rs relayState
	if json.Unmarshal(raw, &rs) != nil {
		return
	}
	domain := rs.PublicHost
	if i := strings.Index(domain, "."); i >= 0 {
		domain = domain[i+1:]
	}

	seen := map[string]bool{}
	for _, t := range rs.Tunnels {
		key := t.ID + "@" + t.Since.String()
		seen[key] = true
		s := tr.sessions[key]
		if s == nil {
			s = &liveSession{ID: t.ID, Port: t.Port, Since: t.Since, players: map[string]bool{}}
			tr.sessions[key] = s
			if !first {
				tr.st.event("server_online", map[string]any{"id": t.ID, "port": t.Port, "name": t.Name, "bedrock": t.UDP})
			}
		}
		s.Name, s.Bedrock, s.Listed = t.Name, t.UDP, t.Listing != nil
		if t.Listing != nil {
			s.Title, s.Mode, s.Version = t.Listing.Title, t.Listing.Mode, t.Listing.Version
		}
		for _, ip := range t.Players {
			s.players[ip] = true
		}
		if n := len(t.Players) + t.UDPSessions; n > s.Peak {
			s.Peak = n
		}
		if t.Peak > s.Peak {
			s.Peak = t.Peak
		}
		if t.UDPSessions > s.PeakBedrock {
			s.PeakBedrock = t.UDPSessions
		}
		s.BytesIn, s.BytesOut, s.Conns = t.BytesIn, t.BytesOut, t.Conns
	}
	now := time.Now()
	for key, s := range tr.sessions {
		if seen[key] {
			continue
		}
		delete(tr.sessions, key)
		address := ""
		if s.Name != "" {
			address = s.Name + "." + domain
		} else {
			address = rs.PublicHost + ":" + itoa(s.Port)
		}
		tr.st.record("sessions.jsonl", map[string]any{
			"t": now.UnixMilli(), "id": s.ID, "port": s.Port, "name": s.Name, "address": address,
			"title": s.Title, "mode": s.Mode, "version": s.Version, "listed": s.Listed, "bedrock": s.Bedrock,
			"start": s.Since.UnixMilli(), "end": now.UnixMilli(), "duration_s": int64(now.Sub(s.Since).Seconds()),
			"unique_players": len(s.players), "peak_players": s.Peak, "peak_bedrock": s.PeakBedrock,
			"connections": s.Conns, "bytes_in": s.BytesIn, "bytes_out": s.BytesOut,
		})
		tr.st.event("server_offline", map[string]any{"id": s.ID, "port": s.Port, "name": s.Name, "duration_s": int64(now.Sub(s.Since).Seconds())})
	}

	// Custom-address claims and releases.
	names := map[string]string{}
	for _, n := range rs.Names {
		names[n.Name] = n.ID
		if _, ok := tr.names[n.Name]; !ok && !first {
			tr.st.event("address_claimed", map[string]any{"name": n.Name, "id": n.ID, "port": n.Port})
		}
	}
	for name, id := range tr.names {
		if _, ok := names[name]; !ok {
			tr.st.event("address_released", map[string]any{"name": name, "id": id})
		}
	}
	tr.names = names
}

// liveSessions returns the sessions in progress, for the dashboard.
func (tr *tracker) live() []map[string]any {
	out := []map[string]any{}
	for _, s := range tr.sessions {
		out = append(out, map[string]any{"id": s.ID, "since": s.Since.UnixMilli(), "unique_players": len(s.players), "peak_players": s.Peak})
	}
	return out
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

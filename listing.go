package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// The "Powered by VoxelPort" server list: a host that opts in sends a listing
// frame, and while its tunnel is up it appears at /api/servers. Nothing is
// persisted — a listing disappears the moment its tunnel closes.

// Listing is what a host publishes about its server.
type Listing struct {
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Version     string `json:"version,omitempty"`
	Mode        string `json:"mode,omitempty"`
	Players     int    `json:"players"`
	MaxPlayers  int    `json:"max_players"`
	Bedrock     bool   `json:"bedrock,omitempty"`
}

var listingModes = map[string]bool{
	"": true, "survival": true, "creative": true, "adventure": true,
	"hardcore": true, "modded": true, "minigames": true, "skyblock": true,
}

const maxListedServers = 200

// cleanText strips control characters and Minecraft § formatting codes,
// collapses whitespace and truncates to max runes.
func cleanText(s string, max int) string {
	var b strings.Builder
	space := false
	skipNext := false
	for _, r := range s {
		if skipNext {
			skipNext = false
			continue
		}
		if r == '§' {
			skipNext = true
			continue
		}
		if unicode.IsSpace(r) {
			space = true
			continue
		}
		if unicode.IsControl(r) || r == utf8.RuneError {
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	out := b.String()
	if utf8.RuneCountInString(out) > max {
		out = string([]rune(out)[:max])
	}
	return strings.TrimSpace(out)
}

// sanitizeListing returns a cleaned copy of l, or a user-facing error.
func sanitizeListing(l Listing) (Listing, error) {
	out := Listing{
		Title:       cleanText(l.Title, 40),
		Description: cleanText(l.Description, 160),
		Version:     cleanText(l.Version, 24),
		Mode:        strings.ToLower(cleanText(l.Mode, 16)),
		Bedrock:     l.Bedrock,
	}
	if utf8.RuneCountInString(out.Title) < 3 {
		return Listing{}, errors.New("Give your server a name of at least 3 characters to list it.")
	}
	if containsBlockedWord(out.Title) || containsBlockedWord(out.Description) {
		return Listing{}, errors.New("Your server's name or description has words that aren't allowed on the public list.")
	}
	if !listingModes[out.Mode] {
		out.Mode = ""
	}
	out.MaxPlayers = min(max(l.MaxPlayers, 1), 1000)
	out.Players = min(max(l.Players, 0), out.MaxPlayers)
	return out, nil
}

// hiddenList is an operator takedown file: one entry per line, either a
// custom-address name or the first 8+ hex characters of a token's ID (as
// printed in the relay logs). Blank lines and # comments are ignored. The
// file is re-read every minute, so edits apply without a restart.
type hiddenList struct {
	path string

	mu      sync.Mutex
	entries []string
	loaded  time.Time
}

func newHiddenList(path string) *hiddenList {
	return &hiddenList{path: path}
}

func (h *hiddenList) current() []string {
	if h == nil || h.path == "" {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if time.Since(h.loaded) < time.Minute {
		return h.entries
	}
	h.loaded = time.Now()
	f, err := os.Open(h.path)
	if err != nil {
		h.entries = nil
		return nil
	}
	defer f.Close()
	var entries []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.ToLower(strings.TrimSpace(sc.Text()))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		entries = append(entries, line)
	}
	h.entries = entries
	return entries
}

func (h *hiddenList) hides(clientID, name string) bool {
	for _, e := range h.current() {
		if (name != "" && e == name) || (len(e) >= 8 && strings.HasPrefix(clientID, e)) {
			return true
		}
	}
	return false
}

type serverEntry struct {
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Version     string `json:"version,omitempty"`
	Mode        string `json:"mode,omitempty"`
	Players     int    `json:"players"`
	MaxPlayers  int    `json:"max_players"`
	Address     string `json:"address"`
	Bedrock     bool   `json:"bedrock"`
	BedrockHost string `json:"bedrock_host,omitempty"`
	BedrockPort int    `json:"bedrock_port,omitempty"`
	UptimeMin   int    `json:"uptime_minutes"`
}

// apiServers lists every live tunnel whose host opted into the public list.
func (r *relay) apiServers(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	clients := make([]*client, 0, len(r.clients))
	for _, c := range r.clients {
		clients = append(clients, c)
	}
	r.mu.Unlock()

	entries := make([]serverEntry, 0)
	for _, c := range clients {
		c.mu.Lock()
		l := c.listing
		c.mu.Unlock()
		if l == nil {
			continue
		}
		name := ""
		if r.names != nil {
			name = r.names.nameFor(c.id)
		}
		if r.hidden.hides(c.id, name) {
			continue
		}
		e := serverEntry{
			Title: l.Title, Description: l.Description, Version: l.Version, Mode: l.Mode,
			Players: l.Players, MaxPlayers: l.MaxPlayers,
			Address:   fmt.Sprintf("%s:%d", r.publicHost, c.publicPort),
			UptimeMin: int(time.Since(c.startedAt).Minutes()),
		}
		host := r.publicHost
		if name != "" {
			e.Address = r.names.address(name)
			host = e.Address
		}
		if l.Bedrock && c.udp != nil {
			e.Bedrock, e.BedrockHost, e.BedrockPort = true, host, c.publicPort
		}
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Players != entries[j].Players {
			return entries[i].Players > entries[j].Players
		}
		return entries[i].UptimeMin > entries[j].UptimeMin
	})
	if len(entries) > maxListedServers {
		entries = entries[:maxListedServers]
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "public, max-age=15")
	json.NewEncoder(w).Encode(map[string]any{"servers": entries})
}

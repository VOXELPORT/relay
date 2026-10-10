package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Custom addresses: a host claims "steve" and players join steve.voxelport.in
// with no port. The relay publishes two DNS records per name — an SRV record
// (_minecraft._tcp.steve → play.voxelport.in:<port>, which Java clients
// follow) and a CNAME (steve → play.voxelport.in, so Bedrock players can type
// the name plus the port). Because DNS points at a fixed port, a claimed name
// permanently reserves that port for its token, even while the host is
// offline, until the name is released or expires from disuse.

const (
	nameExpiry          = 60 * 24 * time.Hour // unused this long → released
	nameTouchInterval   = time.Hour           // LastSeen is persisted at most this often
	nameClaimLimit      = 5                   // claim attempts per client…
	nameClaimWindow     = 10 * time.Minute    // …per this window
	nameDNSTimeout      = 20 * time.Second
	nameExpirySweepTick = 24 * time.Hour
)

var namePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{1,18}[a-z0-9])$`)

// reservedNames can never be claimed: existing infrastructure, things that
// could pass for official VoxelPort/Mojang/Microsoft addresses, and generic
// service names.
var reservedNames = map[string]bool{}

func init() {
	for _, n := range strings.Fields(`
		www relay direct play api mail smtp imap pop ftp ssh vpn ns ns1 ns2 dns
		admin administrator root status docs doc app apps store download downloads
		servers server support help founder n voxelport voxel official staff team
		mod mods moderator owner cdn static assets img images blog dev test beta
		staging login auth account accounts billing pay payment discord minecraft
		mojang microsoft xbox java bedrock realms hypixel localhost info news
		security abuse report legal privacy terms`) {
		reservedNames[n] = true
	}
}

// blockedWords are refused anywhere inside a name or a public listing — a
// deliberately small list of words that are almost never innocent inside
// another word (no "sex", "cock" or "rape": Essex, peacock, grapes).
var blockedWords = []string{
	"fuck", "shit", "cunt", "bitch", "nigg", "fagg", "porn", "hentai", "nazi",
	"hitler", "retard", "whore", "slut",
}

func containsBlockedWord(s string) bool {
	folded := strings.NewReplacer("0", "o", "1", "i", "3", "e", "4", "a", "5", "s", "7", "t", "@", "a", "$", "s", "-", "", "_", "", " ", "").
		Replace(strings.ToLower(s))
	for _, w := range blockedWords {
		if strings.Contains(folded, w) {
			return true
		}
	}
	return false
}

// validateName returns a user-facing error, or nil if name is claimable in
// principle (availability is checked separately).
func validateName(name string) error {
	if !namePattern.MatchString(name) {
		return errors.New("Use 3–20 lowercase letters, numbers or dashes (not at the start or end).")
	}
	if strings.Contains(name, "--") {
		return errors.New("Two dashes in a row aren't allowed.")
	}
	if reservedNames[name] {
		return errors.New("That name is reserved — try another.")
	}
	if containsBlockedWord(name) {
		return errors.New("That name isn't allowed — try another.")
	}
	return nil
}

// dnsProvider publishes and removes the records behind a custom address.
// record IDs are opaque to the store and kept so records can be updated or
// deleted later.
type dnsProvider interface {
	Exists(ctx context.Context, name string) (bool, error)
	Create(ctx context.Context, name string, port int) ([]string, error)
	Repoint(ctx context.Context, records []string, name string, port int) error
	Delete(ctx context.Context, records []string) error
}

type nameEntry struct {
	Name     string    `json:"name"`
	ClientID string    `json:"client_id"`
	Port     int       `json:"port"`
	Records  []string  `json:"records"`
	Created  time.Time `json:"created"`
	LastSeen time.Time `json:"last_seen"`
}

type nameStore struct {
	path   string
	domain string
	dns    dnsProvider

	mu       sync.Mutex
	byName   map[string]*nameEntry
	byClient map[string]*nameEntry
	byPort   map[int]string // port -> clientID
	attempts map[string][]time.Time

	// dnsMu serialises every operation that talks to DNS, so two claims for
	// the same name can't both pass the availability check.
	dnsMu sync.Mutex
}

// loadNameStore reads path (a missing file is an empty store).
func loadNameStore(path, domain string, dns dnsProvider) (*nameStore, error) {
	s := &nameStore{
		path:     path,
		domain:   domain,
		dns:      dns,
		byName:   map[string]*nameEntry{},
		byClient: map[string]*nameEntry{},
		byPort:   map[int]string{},
		attempts: map[string][]time.Time{},
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var entries []*nameEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	for _, e := range entries {
		if e == nil || validateName(e.Name) != nil || e.ClientID == "" || e.Port <= 0 {
			continue
		}
		s.index(e)
	}
	return s, nil
}

func (s *nameStore) index(e *nameEntry) {
	s.byName[e.Name] = e
	s.byClient[e.ClientID] = e
	s.byPort[e.Port] = e.ClientID
}

func (s *nameStore) unindex(e *nameEntry) {
	delete(s.byName, e.Name)
	delete(s.byClient, e.ClientID)
	if s.byPort[e.Port] == e.ClientID {
		delete(s.byPort, e.Port)
	}
}

// saveLocked writes the store atomically. Callers hold s.mu.
func (s *nameStore) saveLocked() error {
	entries := make([]*nameEntry, 0, len(s.byName))
	for _, e := range s.byName {
		entries = append(entries, e)
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	suffix := make([]byte, 4)
	rand.Read(suffix)
	tmp := filepath.Join(filepath.Dir(s.path), fmt.Sprintf(".%s.%x.tmp", filepath.Base(s.path), suffix))
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *nameStore) address(name string) string {
	return name + "." + s.domain
}

func (s *nameStore) forClient(clientID string) (nameEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byClient[clientID]
	if !ok {
		return nameEntry{}, false
	}
	return *e, true
}

func (s *nameStore) nameFor(clientID string) string {
	e, ok := s.forClient(clientID)
	if !ok {
		return ""
	}
	return e.Name
}

// portOwner returns the client whose custom address reserves port, or "".
func (s *nameStore) portOwner(port int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byPort[port]
}

func (s *nameStore) allowAttempt(clientID string) bool {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.attempts[clientID][:0]
	for _, t := range s.attempts[clientID] {
		if now.Sub(t) < nameClaimWindow {
			kept = append(kept, t)
		}
	}
	if len(kept) >= nameClaimLimit {
		s.attempts[clientID] = kept
		return false
	}
	s.attempts[clientID] = append(kept, now)
	return true
}

// claim gives clientID the custom address name, pointing at port (the port
// its live tunnel holds right now). A client has at most one name; claiming
// a new one releases the old.
func (s *nameStore) claim(clientID, name string, port int) (nameEntry, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if err := validateName(name); err != nil {
		return nameEntry{}, err
	}
	if !s.allowAttempt(clientID) {
		return nameEntry{}, errors.New("Too many tries — wait a few minutes and try again.")
	}

	s.dnsMu.Lock()
	defer s.dnsMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), nameDNSTimeout)
	defer cancel()

	s.mu.Lock()
	existing := s.byName[name]
	old := s.byClient[clientID]
	s.mu.Unlock()

	if existing != nil && existing.ClientID != clientID {
		return nameEntry{}, errors.New("That name is taken — try another.")
	}
	if existing != nil { // already ours
		if existing.Port != port {
			if err := s.dns.Repoint(ctx, existing.Records, name, port); err != nil {
				log.Printf("[names] repoint %s: %v", name, err)
				return nameEntry{}, errors.New("Couldn't update your address right now — try again shortly.")
			}
			s.mu.Lock()
			s.unindex(existing)
			existing.Port = port
			s.index(existing)
			s.saveLocked()
			s.mu.Unlock()
		}
		s.mu.Lock()
		out := *existing
		s.mu.Unlock()
		return out, nil
	}

	taken, err := s.dns.Exists(ctx, name)
	if err != nil {
		log.Printf("[names] lookup %s: %v", name, err)
		return nameEntry{}, errors.New("Couldn't check that name right now — try again shortly.")
	}
	if taken {
		return nameEntry{}, errors.New("That name is taken — try another.")
	}
	records, err := s.dns.Create(ctx, name, port)
	if err != nil {
		log.Printf("[names] create %s: %v", name, err)
		return nameEntry{}, errors.New("Couldn't set up that address right now — try again shortly.")
	}

	now := time.Now()
	e := &nameEntry{Name: name, ClientID: clientID, Port: port, Records: records, Created: now, LastSeen: now}
	s.mu.Lock()
	if old != nil {
		s.unindex(old)
	}
	s.index(e)
	if err := s.saveLocked(); err != nil {
		log.Printf("[names] save: %v", err)
	}
	out := *e
	s.mu.Unlock()

	if old != nil {
		if err := s.dns.Delete(ctx, old.Records); err != nil {
			log.Printf("[names] delete old %s: %v", old.Name, err)
		}
	}
	log.Printf("[names] %s… claimed %s -> port %d", clientID[:8], name, port)
	return out, nil
}

// release removes clientID's custom address (a no-op if it has none).
func (s *nameStore) release(clientID string) error {
	s.dnsMu.Lock()
	defer s.dnsMu.Unlock()
	s.mu.Lock()
	e := s.byClient[clientID]
	s.mu.Unlock()
	if e == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), nameDNSTimeout)
	defer cancel()
	if err := s.dns.Delete(ctx, e.Records); err != nil {
		log.Printf("[names] delete %s: %v", e.Name, err)
		return errors.New("Couldn't remove your address right now — try again shortly.")
	}
	s.mu.Lock()
	s.unindex(e)
	s.saveLocked()
	s.mu.Unlock()
	log.Printf("[names] %s… released %s", clientID[:8], e.Name)
	return nil
}

// repoint moves clientID's address to port (used when its reserved port
// couldn't be bound and the tunnel got a different one).
func (s *nameStore) repoint(clientID string, port int) {
	s.dnsMu.Lock()
	defer s.dnsMu.Unlock()
	s.mu.Lock()
	e := s.byClient[clientID]
	s.mu.Unlock()
	if e == nil || e.Port == port {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), nameDNSTimeout)
	defer cancel()
	if err := s.dns.Repoint(ctx, e.Records, e.Name, port); err != nil {
		log.Printf("[names] repoint %s: %v", e.Name, err)
		return
	}
	s.mu.Lock()
	s.unindex(e)
	e.Port = port
	s.index(e)
	s.saveLocked()
	s.mu.Unlock()
}

// touch records that clientID's address is still in use.
func (s *nameStore) touch(clientID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.byClient[clientID]
	if e == nil || time.Since(e.LastSeen) < nameTouchInterval {
		return
	}
	e.LastSeen = time.Now()
	s.saveLocked()
}

// sweepExpired releases names unused for longer than maxAge. isLive reports
// whether a client currently has a tunnel up (those are never expired).
func (s *nameStore) sweepExpired(maxAge time.Duration, isLive func(clientID string) bool) {
	s.mu.Lock()
	var stale []string
	for id, e := range s.byClient {
		if time.Since(e.LastSeen) > maxAge && !isLive(id) {
			stale = append(stale, id)
		}
	}
	s.mu.Unlock()
	for _, id := range stale {
		if err := s.release(id); err != nil {
			log.Printf("[names] expire %s…: %v", id[:8], err)
		}
	}
}

func (r *relay) nameSweepLoop() {
	for range time.Tick(nameExpirySweepTick) {
		r.names.sweepExpired(nameExpiry, func(id string) bool {
			r.mu.Lock()
			defer r.mu.Unlock()
			_, live := r.clients[id]
			return live
		})
	}
}

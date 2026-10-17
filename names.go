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
// reserves that port for its token while the host is away, for the port grace
// period (a week by default) after it was last seen. After that the port goes back
// to the pool; when the host returns and the port is taken, it gets another one and
// the DNS record follows it, so the address players use doesn't change. A name
// itself lives much longer (nameExpiry), and supporters' and granted addresses keep
// their port for good.

const (
	nameExpiry          = 60 * 24 * time.Hour // unused this long → released
	nameTouchInterval   = time.Hour           // LastSeen is persisted at most this often
	nameClaimLimit      = 5                   // claim attempts per client…
	nameClaimWindow     = 10 * time.Minute    // …per this window
	nameDNSTimeout      = 20 * time.Second
	nameExpirySweepTick = 24 * time.Hour

	// How long a name keeps its port reserved after its host was last seen (-name-port-grace).
	defaultNamePortGrace = 7 * 24 * time.Hour
	// At most this share of the port range may be reserved by custom addresses (-max-reserved-port-fraction),
	// so offline names can never starve the hosts that are online.
	defaultMaxReservedFraction = 0.5
	// Successful new claims per source address per day, for IPv4 and for an IPv6 /64 (see limitKey).
	defaultNameClaimsPerIPDay  = 10
	defaultNameClaimsPerIPDay6 = 3
	nameClaimsDay              = 24 * time.Hour
	// Writing LastSeen to disk when a host disconnects is skipped if the file was written this recently.
	nameCloseSaveEvery = 5 * time.Minute
)

// errNamesSupportersOnly is what a host is told while new custom addresses are a supporter perk (-names-supporters-only). It
// says both things a host needs to know: why nothing new, and that the address they already have keeps working.
var errNamesSupportersOnly = errors.New("New custom addresses are for VoxelPort supporters now. Your current address (if you have one) keeps working. / Naye custom address ab sirf VoxelPort supporters ke liye hain; jo address pehle se hai woh chalta rahega.")

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

var shortNamePattern = regexp.MustCompile(`^[a-z0-9]{1,2}$`)

// validateShortName checks a 1–2 character address, which only the operator can hand out.
func validateShortName(name string) error {
	if !shortNamePattern.MatchString(name) {
		return errors.New("A short name is 1–2 lowercase letters or numbers.")
	}
	if reservedNames[name] {
		return errors.New("That name is reserved.")
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
	// Granted marks an address the operator gave this host (a supporter's short address). A host's
	// own claim can't replace it, so an app that still remembers an older address can't undo it.
	Granted bool `json:"granted,omitempty"`
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

	// Capacity rules (see nameLimits). Zero values mean the defaults above.
	grace               time.Duration
	maxReservedFraction float64
	poolSize            int // ports in the relay's range; 0 = unknown, so no cap
	keep                func(clientID, name string) bool
	claimsPerIPDay      int
	claimsPerIPDay6     int
	supportersOnly      bool                   // set by -names-supporters-only: ordinary hosts get no new address; existing ones, re-claims and releases still work
	ipClaims            map[string][]time.Time // limitKey -> times of successful new claims in the last day
	lastSaved           time.Time

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
		ipClaims: map[string][]time.Time{},
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
		if e == nil || e.ClientID == "" || e.Port <= 0 {
			continue
		}
		// 1–2 character addresses can only come from the operator (grant_name), so they are kept
		// (and marked as granted, which older saves didn't record) rather than dropped as invalid.
		if validateName(e.Name) != nil {
			if validateShortName(e.Name) != nil {
				continue
			}
			e.Granted = true
		}
		s.index(e)
	}
	return s, nil
}

// nameLimits are the capacity rules of the store; the relay sets them once at start.
type nameLimits struct {
	Grace               time.Duration                    // how long an absent host keeps its port
	MaxReservedFraction float64                          // share of the range names may reserve
	PoolSize            int                              // ports in the range
	Keep                func(clientID, name string) bool // supporters: port kept for good, and exempt from the cap
	ClaimsPerIPDay      int                              // new claims per IPv4 address per day
	ClaimsPerIPDay6     int                              // new claims per IPv6 /64 per day
	SupportersOnly      bool                             // new custom addresses only for supporters and operator grants
}

func (s *nameStore) configure(l nameLimits) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grace, s.maxReservedFraction, s.poolSize, s.keep = l.Grace, l.MaxReservedFraction, l.PoolSize, l.Keep
	s.claimsPerIPDay, s.claimsPerIPDay6 = l.ClaimsPerIPDay, l.ClaimsPerIPDay6
	s.supportersOnly = l.SupportersOnly
}

// The effective rules, with defaults for anything left at zero. Callers hold s.mu.
func (s *nameStore) graceLocked() time.Duration {
	if s.grace <= 0 {
		return defaultNamePortGrace
	}
	return s.grace
}

func (s *nameStore) fractionLocked() float64 {
	if s.maxReservedFraction <= 0 {
		return defaultMaxReservedFraction
	}
	return s.maxReservedFraction
}

func (s *nameStore) claimsPerDayLocked(key string) int {
	if isIPv6Key(key) {
		if s.claimsPerIPDay6 > 0 {
			return s.claimsPerIPDay6
		}
		return defaultNameClaimsPerIPDay6
	}
	if s.claimsPerIPDay > 0 {
		return s.claimsPerIPDay
	}
	return defaultNameClaimsPerIPDay
}

// reservesLocked reports whether e's port is held for it right now: always for a supporter or a granted
// address, otherwise for the grace period after the host was last seen. Callers hold s.mu.
func (s *nameStore) reservesLocked(e *nameEntry, now time.Time) bool {
	if e.Granted || (s.keep != nil && s.keep(e.ClientID, e.Name)) {
		return true
	}
	return now.Sub(e.LastSeen) <= s.graceLocked()
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
	s.lastSaved = time.Now()
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

// portOwner returns the client whose custom address points at port, or "" (whether or not it is still reserved).
func (s *nameStore) portOwner(port int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byPort[port]
}

// portHeldFrom reports whether port is reserved for somebody other than clientID: its name's owner is a supporter
// or has a granted address, or was last seen within the grace period. A port whose owner has been away longer
// is free for anyone.
func (s *nameStore) portHeldFrom(port int, clientID string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	owner := s.byPort[port]
	if owner == "" || owner == clientID {
		return false
	}
	e := s.byClient[owner]
	return e != nil && s.reservesLocked(e, now)
}

// reservedCount is how many names hold their port right now.
func (s *nameStore) reservedCount(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reservedCountLocked(now)
}

func (s *nameStore) reservedCountLocked(now time.Time) int {
	n := 0
	for _, e := range s.byName {
		if s.reservesLocked(e, now) {
			n++
		}
	}
	return n
}

// recentClaimsLocked is the successful new claims from key within the last day (older ones are dropped). Callers hold s.mu.
func (s *nameStore) recentClaimsLocked(key string, now time.Time) []time.Time {
	kept := s.ipClaims[key][:0]
	for _, t := range s.ipClaims[key] {
		if now.Sub(t) < nameClaimsDay {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(s.ipClaims, key)
	} else {
		s.ipClaims[key] = kept
	}
	return kept
}

// sweepIPClaims drops source addresses with no claims in the last day, so the map can't grow under address-rotating abuse.
func (s *nameStore) sweepIPClaims() {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.ipClaims {
		s.recentClaimsLocked(key, now)
	}
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
	return s.claimName(clientID, "", name, port, false)
}

// claimFrom is claim, counting the claim against the host's source address (see limitKey).
func (s *nameStore) claimFrom(clientID, ip, name string, port int) (nameEntry, error) {
	return s.claimName(clientID, ip, name, port, false)
}

// grantShort gives clientID a 1–2 character address. Only the operator can do this
// (the dashboard's "Short name" action); hosts can't claim one themselves.
func (s *nameStore) grantShort(clientID, name string, port int) (nameEntry, error) {
	return s.claimName(clientID, "", name, port, true)
}

func (s *nameStore) claimName(clientID, ip, name string, port int, short bool) (nameEntry, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	check := validateName
	if short {
		check = validateShortName
	}
	// An address the host already holds is always fine to "claim" again (apps re-send it on every
	// start), even a short one that an ordinary claim couldn't make.
	s.mu.Lock()
	own := s.byClient[clientID]
	s.mu.Unlock()
	if own == nil || own.Name != name {
		if err := check(name); err != nil {
			return nameEntry{}, err
		}
	}
	// The host's own claim attempts are rate-limited; the operator's grants are not.
	if !short && !s.allowAttempt(clientID) {
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
	if !short && old != nil && old.Granted && old.Name != name {
		return nameEntry{}, errors.New("Your address was set for you by VoxelPort (a supporter perk), so it can't be replaced. Remove it first if you'd like a different one.")
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

	// A brand-new name from here on. When custom addresses are a supporter perk (every address is DNS records, and the zone
	// has a fixed number of them), only supporters and the operator's own grants get a new one. Names that already exist,
	// re-claiming one, and releasing one are not affected.
	s.mu.Lock()
	supportersOnly := s.supportersOnly
	isSupporter := s.keep != nil && s.keep(clientID, name)
	s.mu.Unlock()
	if supportersOnly && !short && !isSupporter {
		return nameEntry{}, errNamesSupportersOnly
	}

	// The operator's grants and supporters are exempt from the capacity rules; everyone
	// else is held to the share of the port range names may reserve, and to a daily limit per source address, so a few
	// scripts can't squat the pool.
	now := time.Now()
	key := ""
	if !short && ip != "" {
		key = limitKey(ip)
	}
	if !short {
		s.mu.Lock()
		exempt := s.keep != nil && s.keep(clientID, name)
		full := !exempt && old == nil && s.poolSize > 0 && float64(s.reservedCountLocked(now)) >= s.fractionLocked()*float64(s.poolSize)
		over := false
		if key != "" && !exempt {
			over = len(s.recentClaimsLocked(key, now)) >= s.claimsPerDayLocked(key)
		}
		s.mu.Unlock()
		if full {
			return nameEntry{}, errors.New("Custom addresses are full right now — try again later.")
		}
		if over {
			return nameEntry{}, errors.New("Too many custom addresses were claimed from your network today — try again tomorrow.")
		}
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

	now = time.Now()
	e := &nameEntry{Name: name, ClientID: clientID, Port: port, Records: records, Created: now, LastSeen: now, Granted: short}
	s.mu.Lock()
	if old != nil {
		s.unindex(old)
	}
	s.index(e)
	if key != "" {
		s.ipClaims[key] = append(s.recentClaimsLocked(key, now), now)
	}
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

// touchClosed records that clientID's host has just gone away, which is when its port grace starts counting. LastSeen is
// only written on connect (and hourly), so without this a host that stayed online for days would have its port released
// the moment it dropped. The change is kept in memory at once and written to disk at most every few minutes.
func (s *nameStore) touchClosed(clientID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.byClient[clientID]
	if e == nil {
		return
	}
	e.LastSeen = time.Now()
	if time.Since(s.lastSaved) >= nameCloseSaveEvery {
		s.saveLocked()
	}
}

// sweepExpired releases names unused for longer than maxAge. isLive reports
// whether a client currently has a tunnel up (those are never expired).
func (s *nameStore) sweepExpired(maxAge time.Duration, isLive func(clientID string) bool) {
	s.sweepExpiredKeeping(maxAge, isLive, nil)
}

// sweepExpiredKeeping is sweepExpired, except addresses for which keep(clientID, name)
// is true are never expired (supporters' addresses). keep must not call back into s.
func (s *nameStore) sweepExpiredKeeping(maxAge time.Duration, isLive func(clientID string) bool, keep func(clientID, name string) bool) {
	s.mu.Lock()
	var stale []string
	for id, e := range s.byClient {
		if time.Since(e.LastSeen) > maxAge && !isLive(id) && (keep == nil || !keep(id, e.Name)) {
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
		r.names.sweepIPClaims()
		r.names.sweepExpiredKeeping(nameExpiry, func(id string) bool {
			r.mu.Lock()
			defer r.mu.Unlock()
			_, live := r.clients[id]
			return live
		}, r.supporters.has) // a supporter's address is kept even when they haven't hosted for a while
	}
}

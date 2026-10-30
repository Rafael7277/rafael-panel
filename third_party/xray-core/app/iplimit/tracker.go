// Package iplimit implements the Rafael panel's in-process IP limit gate
// (iplimit.md phase 2, section 14/15).
//
// The panel writes a JSON limits file (path passed via the XRAY_IPLIMIT_FILE
// environment variable to the Xray child process) describing, per client
// email and logical group, the maximum number of simultaneous distinct source IPs. The gate is
// consulted from DefaultDispatcher.routedDispatch — the single convergence
// point of all proxy protocols, where session.Inbound.User and
// session.Inbound.Source are already populated and no user byte has been
// relayed yet.
//
// Design contract (iplimit.md §16 fail-closed for user policy, §17 coexistence):
//
//   - A panel-managed process requires a complete, valid policy snapshot.
//     Until the first snapshot loads, or for an unknown user in strict mode,
//     reject rather than accidentally bypassing a configured limit.
//   - An explicitly unlimited user and an empty email (dokodemo/API/
//     healthcheck traffic) => ALLOW.
//   - Counting is connection-synchronous: Check is called before dispatch,
//     and the returned release callback runs when the authenticated context
//     ends — no access-log delay.
//   - One (logical identity, source IP) pair holds a slot while it has live connections.
//     Multiple flows from that IP share the slot; a different IP is rejected
//     immediately when the limit is full.
//   - The whole package is additive (upstream files untouched except the
//     marked hook in app/dispatcher/default.go) to keep upstream merges
//     conflict-free.
package iplimit

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"
)

// defaultTTL bounds the in-memory retention of released slots. Released
// slots do not count toward the limit, so this is not a connection grace
// period and does not delay admission from a different IP.
const defaultTTL = 60 * time.Second

// limitEntry is one client's limit as written by the panel.
type limitEntry struct {
	Identity string `json:"identity,omitempty"`
	LimitIP  int64  `json:"limitIp"`
	Disabled bool   `json:"disabled,omitempty"`
}

// limitsFile is the JSON shape of the limits file.
type limitsFile struct {
	Strict bool                  `json:"strict,omitempty"`
	Limits map[string]limitEntry `json:"limits"`
}

// slot is one active (logical identity, ip) reservation.
type slot struct {
	lastActive time.Time
	// conns counts live connections currently holding this slot. The slot
	// may only be garbage-collected when conns == 0 and the TTL expired.
	conns int
}

// Tracker is the in-process IP limit gate. All methods are safe for
// concurrent use.
type Tracker struct {
	mu sync.Mutex
	// reloadMu serializes file reads so two concurrent first connections cannot
	// publish snapshots out of order while the panel atomically replaces it.
	reloadMu sync.Mutex
	// limits maps normalized email -> configured policy. nil entries mean "no limit".
	limits map[string]limitEntry
	// active maps a logical client identity -> (ip -> slot). Policies for
	// every alias in a multi-link group point at the same identity.
	active map[string]map[string]*slot

	filePath string
	// loaded and strict describe the last complete policy snapshot. When a
	// panel configured a policy path, a missing first snapshot is fail-closed.
	loaded bool
	strict bool
	// loadedAt tracks the limits file mtime to detect panel rewrites.
	fileMod  time.Time
	fileLoad time.Time
}

var (
	globalOnce sync.Once
	global     *Tracker
)

// Global returns the process-wide tracker. It is created lazily on first
// use; Xray child processes without XRAY_IPLIMIT_FILE keep a tracker with
// no policy enforcement at effectively zero cost.
func Global() *Tracker {
	globalOnce.Do(func() {
		global = &Tracker{
			limits: make(map[string]limitEntry),
			active: make(map[string]map[string]*slot),
		}
		global.startFileWatch()
	})
	return global
}

// startFileWatch reads the limits file once at startup and then refreshes
// it whenever its mtime changes, checked on each new connection (cheap stat)
// and on a slow ticker as a fallback. Missing, malformed, or incomplete policy
// data closes the gate until a complete strict snapshot is loaded.
func (t *Tracker) startFileWatch() {
	t.filePath = os.Getenv("XRAY_IPLIMIT_FILE")
	if t.filePath == "" {
		return
	}
	t.reload(false)
	go func() {
		policyTicker := time.NewTicker(10 * time.Second)
		cleanupTicker := time.NewTicker(defaultTTL)
		defer policyTicker.Stop()
		defer cleanupTicker.Stop()
		for {
			select {
			case <-policyTicker.C:
				t.reloadIfChanged()
			case now := <-cleanupTicker.C:
				t.sweepExpired(now)
			}
		}
	}()
}

// reloadIfChanged re-reads the limits file when its mtime moved.
func (t *Tracker) reloadIfChanged() {
	if t.filePath == "" {
		return
	}
	fi, err := os.Stat(t.filePath)
	if err != nil {
		// A configured policy disappearing or becoming unreadable must not
		// leave the last permissive snapshot active.
		t.mu.Lock()
		t.loaded = false
		t.fileMod = time.Time{}
		t.mu.Unlock()
		return
	}
	t.mu.Lock()
	unchanged := !fi.ModTime().Equal(t.fileMod)
	t.mu.Unlock()
	if !unchanged {
		return
	}
	t.reload(true)
}

// reload parses a complete limits snapshot; invalid content closes the gate.
func (t *Tracker) reload(fromWatcher bool) {
	t.reloadMu.Lock()
	defer t.reloadMu.Unlock()
	before, err := os.Stat(t.filePath)
	if err != nil {
		t.markUnavailable()
		return
	}
	data, err := os.ReadFile(t.filePath)
	if err != nil {
		t.markUnavailable()
		return // Check rejects until a valid snapshot loads
	}
	var lf limitsFile
	if err := json.Unmarshal(data, &lf); err != nil {
		t.markUnavailable()
		return // malformed policy must never keep stale limits active
	}
	if !lf.Strict || lf.Limits == nil {
		t.markUnavailable()
		return // panel-managed snapshots must be explicitly strict and complete
	}
	normalizedLimits := make(map[string]limitEntry, len(lf.Limits))
	for email, entry := range lf.Limits {
		policyEmail := strings.ToLower(strings.TrimSpace(email))
		if policyEmail == "" || entry.LimitIP < 0 {
			t.markUnavailable()
			return // invalid entries must not turn into an unlimited bypass
		}
		if strings.TrimSpace(entry.Identity) == "" {
			entry.Identity = policyEmail
		}
		if previous, ok := normalizedLimits[policyEmail]; ok {
			if previous.Disabled && !entry.Disabled {
				normalizedLimits[policyEmail] = entry
				continue
			}
			if !previous.Disabled && !entry.Disabled && entry.LimitIP > 0 &&
				(previous.LimitIP <= 0 || entry.LimitIP < previous.LimitIP) {
				normalizedLimits[policyEmail] = entry
			}
			continue
		}
		normalizedLimits[policyEmail] = entry
	}
	after, err := os.Stat(t.filePath)
	if err != nil || before.Size() != int64(len(data)) || after.Size() != int64(len(data)) ||
		!before.ModTime().Equal(after.ModTime()) {
		// A replacement raced the read. Leave enforcement closed and let the
		// next connection/watch tick load the complete new file.
		t.markUnavailable()
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	t.limits = normalizedLimits
	t.strict = lf.Strict
	t.loaded = true
	t.fileMod = after.ModTime()
	t.fileLoad = time.Now()
	_ = fromWatcher
}

func (t *Tracker) markUnavailable() {
	t.mu.Lock()
	t.loaded = false
	t.fileMod = time.Time{}
	t.mu.Unlock()
}

// Check is the gate: called with the email and source IP of a NEW
// connection. It returns true when the connection is ALLOWED, and when
// allowed it acquires a slot. The caller MUST call the returned release
// function exactly once when the connection ends (phase-2 hook uses ctx
// cancellation; see hook.go).
//
// Decision order:
//  1. empty email / empty ip                         -> allow (non-user traffic)
//  2. panel policy unavailable or unknown strict user -> reject
//  3. disabled user                                  -> reject
//  4. explicit unlimited user                       -> allow
//  5. ip already holds a live slot                   -> allow (same source IP)
//  6. active distinct IPs < limit                    -> allow
//  7. otherwise                                      -> REJECT
func (t *Tracker) Check(email, ip string, now time.Time) (allowed bool, release func()) {
	policyEmail := strings.ToLower(strings.TrimSpace(email))
	if policyEmail == "" {
		return true, func() {}
	}
	// Apply a panel policy update before deciding this connection. The
	// background watcher remains as a fallback for changes during idle time.
	t.reloadIfChanged()

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.filePath != "" && !t.loaded {
		return false, nil // policy path is configured but no valid snapshot exists
	}

	entry, ok := t.limits[policyEmail]
	if !ok {
		if t.strict {
			return false, nil // do not let an unlisted panel user bypass policy
		}
		return true, func() {}
	}
	if entry.Disabled {
		return false, nil
	}
	if entry.LimitIP <= 0 {
		return true, func() {} // explicitly unlimited
	}
	if ip == "" {
		return false, nil // cannot enforce a limited client's policy without a source IP
	}

	// Reclaim only this client's expired slots on the connection path. A
	// process-wide sweep runs periodically in the background so admission
	// cost does not grow with every other client's retained IP history.
	identity := strings.TrimSpace(entry.Identity)
	if identity == "" {
		identity = policyEmail
	}
	t.gcIdentityLocked(identity, now)

	slots := t.active[identity]
	if slots == nil {
		slots = make(map[string]*slot)
		t.active[identity] = slots
	}

	if s, ok := slots[ip]; ok && s.conns > 0 {
		s.conns++
		s.lastActive = now
		return true, t.releaseFunc(identity, ip)
	}
	// Count distinct IPs currently holding at least one live connection.
	liveDistinct := 0
	for _, s := range slots {
		if s.conns > 0 {
			liveDistinct++
		}
	}
	if int64(liveDistinct) >= entry.LimitIP {
		return false, nil // REJECT — the gate, zero delay
	}

	// An idle slot may still be retained for the TTL, but it no longer owns
	// a seat. Reconnecting from it must pass the same limit check as a new IP.
	slots[ip] = &slot{conns: 1, lastActive: now}
	return true, t.releaseFunc(identity, ip)
}

// releaseFunc builds the release closure for an acquired logical-client slot.
func (t *Tracker) releaseFunc(identity, ip string) func() {
	return func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		slots := t.active[identity]
		if slots == nil {
			return
		}
		s, ok := slots[ip]
		if !ok {
			return
		}
		if s.conns > 0 {
			s.conns--
		}
		if s.conns == 0 {
			// Keep the slot (TTL keeps it warm) but stop counting it as
			// live; gcLocked removes it after the TTL.
			s.lastActive = time.Now()
		}
	}
}

// gcLocked drops slots whose TTL expired and that hold no live connection.
// Called with t.mu held.
func (t *Tracker) gcLocked(now time.Time) {
	for identity := range t.active {
		t.gcIdentityLocked(identity, now)
	}
}

// gcIdentityLocked drops expired, connection-free slots for one identity. Called
// with t.mu held from Check so admission work is proportional to this client's
// recent IPs rather than all clients' retained IPs.
func (t *Tracker) gcIdentityLocked(identity string, now time.Time) {
	slots := t.active[identity]
	if slots == nil {
		return
	}
	ttl := t.ttlLocked()
	for ip, s := range slots {
		if s.conns == 0 && now.Sub(s.lastActive) > ttl {
			delete(slots, ip)
		}
	}
	if len(slots) == 0 {
		delete(t.active, identity)
	}
}

// sweepExpired performs the infrequent process-wide cleanup off the
// connection path. Released slots are retained for defaultTTL, then pruned.
func (t *Tracker) sweepExpired(now time.Time) {
	t.mu.Lock()
	t.gcLocked(now)
	t.mu.Unlock()
}

// ttlLocked returns the TTL for an email. Called with t.mu held.
func (t *Tracker) ttlLocked() time.Duration {
	return defaultTTL
}

// Snapshot returns a copy of the current state (for tests and diagnostics).
type EntrySnapshot struct {
	Email      string
	Identity   string
	IP         string
	Conns      int
	LastActive time.Time
}

func (t *Tracker) Snapshot() []EntrySnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]EntrySnapshot, 0, 16)
	for identity, slots := range t.active {
		for ip, s := range slots {
			out = append(out, EntrySnapshot{
				Email: identity, Identity: identity, IP: ip, Conns: s.conns, LastActive: s.lastActive,
			})
		}
	}
	return out
}

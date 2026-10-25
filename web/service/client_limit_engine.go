// client_limit_engine.go implements the scalable per-client IP limit.
//
// It replaces the old polling job (web/job/check_client_ip_job.go) with an
// event-driven design:
//
//   - An incremental tailer reads only NEW bytes from the Xray access log
//     (never rescans the file, never scans all inbounds), with rotation /
//     truncation detection, partial-line buffering and duplicate resilience.
//   - Client/IP state lives in memory only. No database write happens per
//     observed event; the database stays authoritative for client policy.
//   - Each sighting is active for a grace period (TTL, default 60s, per
//     client via Client.LimitGrace in seconds). An IP that stops being seen
//     expires on its own, so a mobile-network IP change does not accumulate
//     into a permanent violation.
//   - A client violates its limit only while the number of active IPs stays
//     above Client.LimitIP for the whole grace period. Violations are
//     enforced by removing and re-adding the user through the Xray gRPC API,
//     which drops that user's live connections; reconnection from the
//     allowed IPs continues to work.
//   - Enforcement is serialized per email (never a global lock), re-reads
//     the inbound fresh from the database before acting (a client that was
//     just disabled or deleted is never kicked) and writes no DB rows.
//
// The engine has no external dependencies: no fail2ban, no Redis, no extra
// daemon, and it does not modify Xray Core. fail2ban-style [LIMIT_IP] log
// lines are still written to 3xipl.log for operators who run fail2ban on
// Linux, but the engine itself works on any OS, Windows included.
//
// Exact concurrent connection counting is NOT implemented: the Xray access
// log reports source IPs per accepted inbound connection, not open
// connections, so this feature limits the number of distinct active source
// IPs per client, not concurrent connections.
package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/logger"
	"github.com/mhsanaei/3x-ui/v2/xray"
)

const (
	// defaultClientIPGrace is how long an IP stays active after its last
	// sighting when the client sets no explicit grace period (seconds).
	defaultClientIPGrace = 60

	// minClientIPGrace is the smallest per-client grace period the engine
	// honors; smaller configured values are clamped up so a malformed or
	// absurdly tight value cannot effectively disable the grace semantics.
	minClientIPGrace = 10

	// clientIPTailInterval is how often the engine checks the access log for
	// new bytes. Cost per tick is O(new bytes), never O(file) or O(clients).
	clientIPTailInterval = time.Second

	// clientIPCacheTTL bounds how long a client policy (limit, grace, enable)
	// is served from cache before being re-read from the database.
	clientIPCacheTTL = 2 * time.Minute

	// clientIPNegativeCacheTTL bounds how long an unknown email is remembered
	// as "no policy" so log lines with untracked emails cannot cause a query
	// storm. Newly added clients are un-forgotten explicitly on save.
	clientIPNegativeCacheTTL = 30 * time.Second

	// clientIPSweepInterval prunes stale cache/state/lock entries. This is
	// the only periodic scan and it runs rarely by design.
	clientIPSweepInterval = 30 * time.Minute

	// maxClientIPsTracked bounds per-client memory under CGNAT churn or a
	// spoofing flood; the least recently seen IPs are evicted first.
	maxClientIPsTracked = 256

	// clientIPCatchUpCap bounds a one-time re-read after rotation detection
	// so a replaced multi-gigabyte file cannot stall the engine.
	clientIPCatchUpCap = 32 << 20 // 32 MiB

	// clientIPArchiveSize triggers an archive-and-truncate of the access log
	// so it cannot grow unboundedly (the old job cleared it hourly).
	clientIPArchiveSize = 64 << 20 // 64 MiB

	// clientIPArchiveInterval is the maximum age of unarchived access-log
	// content while the log has any bytes.
	clientIPArchiveInterval = time.Hour

	// defaultXrayAPIPort is the fallback gRPC API port when it cannot be
	// resolved from the running config or the DB template.
	defaultXrayAPIPort = 62789
)

// Access-log line patterns (compiled once; the old job recompiled them every
// 10 seconds). A relevant line looks like:
//
//	2026/09/07 12:34:56 tcp:123.45.67.89:54321 accepted email: user@example.com [proxy ...]
var (
	clientIPLimitLineRe  = regexp.MustCompile(`from (?:tcp:|udp:)?\[?([0-9a-fA-F\.:]+)\]?:\d+ accepted`)
	clientIPLimitEmailRe = regexp.MustCompile(`email: (.+)$`)
	clientIPLimitTimeRe  = regexp.MustCompile(`^(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2})`)
)

// ClientIPWithTimestamp is one observed source IP with its last-seen time;
// the same wire format the old job stored in inbound_client_ips and the
// client IP log modal renders.
type ClientIPWithTimestamp struct {
	IP        string `json:"ip"`
	Timestamp int64  `json:"timestamp"`
}

// clientIPSighting is the engine's live view of one source IP for one client.
type clientIPSighting struct {
	firstSeen int64 // unix seconds
	lastSeen  int64 // unix seconds
}

// clientIPState is the live per-client state. Memory only, never persisted.
type clientIPState struct {
	ips          map[string]*clientIPSighting
	pendingSince int64 // unix seconds when the current violation started, 0 = none
	lastKick     int64 // unix seconds of the last kick attempt (success or failure)
	kickFailures int   // consecutive failed enforcement attempts
}

// clientIPPolicy is the cached limit policy of one client.
type clientIPPolicy struct {
	inboundId int
	tag       string
	protocol  string
	limitIP   int
	graceSec  int
	enable    bool // client enabled AND inbound enabled
	loadedAt  int64
}

// clientLimitEngine is the per-UUID IP limit engine. A single instance runs
// for the lifetime of the panel (see StartClientLimitEngine).
type clientLimitEngine struct {
	mu sync.Mutex
	// gateAuthoritative disables the legacy log-based grace/kick enforcement
	// in the production engine. The fork's connection-synchronous gate owns
	// admission; this engine only retains recent IP observations for the UI.
	gateAuthoritative bool

	// now is injectable for tests; production uses time.Now.
	now func() time.Time
	// kickFn is injectable for tests; nil means the real gRPC kick.
	kickFn func(email string, policy *clientIPPolicy) error
	// accessPathFn resolves the access log path; overridable in tests.
	accessPathFn func() (string, error)
	// apiPort overrides Xray API port resolution when > 0 (tests).
	apiPort int

	// limitRouter installs/removes the sticky blackhole rules for
	// violating (email, IP) pairs (design: iplimit.md). Injectable for
	// tests; nil disables sticky enforcement (kick-only mode).
	limitRouter *ClientLimitRouter

	// Tailer state: readOffset is the next unread byte of the access log,
	// partial holds bytes read from the file that do not yet end in a
	// newline, fingerprint is a sample of the file head used to detect that
	// the file was replaced rather than appended to.
	accessPath  string
	readOffset  int64
	partial     []byte
	fingerprint []byte

	// Live state, keyed by client email.
	clients map[string]*clientIPState

	// Policy cache: positive entries (found policy) and negative entries
	// (email known to have no policy, with expiry) so log spam with random
	// emails cannot trigger a DB query per line.
	policy   map[string]*clientIPPolicy
	negative map[string]int64

	// Per-email enforcement locks; never a global lock.
	kickLocksMu sync.Mutex
	kickLocks   map[string]*sync.Mutex

	// Access log archival bookkeeping.
	lastArchive time.Time

	// Counters for diagnostics and tests.
	linesProcessed uint64
	eventsObserved uint64
	kicksExecuted  uint64
	kickFailures   uint64
}

var (
	clientLimitEngineOnce sync.Once
	clientLimitEngineInst *clientLimitEngine
)

// StartClientLimitEngine starts the access-log tailer for recent-IP display.
// The fork's synchronous gate is the sole production admission authority.
// Safe to call multiple times; only one engine runs.
func StartClientLimitEngine() {
	clientLimitEngineOnce.Do(func() {
		clientLimitEngineInst = newClientLimitEngine()
		clientLimitEngineInst.gateAuthoritative = true
		go clientLimitEngineInst.run()
		// Phase-2 gate bootstrap (iplimit.md §16): write the fork's
		// iplimits.json before the Xray child reads it, then keep it
		// fresh through the notify hooks.
		ClientLimitGateRefresh()
	})
}

func newClientLimitEngine() *clientLimitEngine {
	return &clientLimitEngine{
		now:          time.Now,
		accessPathFn: xray.GetAccessLogPath,
		limitRouter:  newClientLimitRouter(),
		clients:      make(map[string]*clientIPState),
		policy:       make(map[string]*clientIPPolicy),
		negative:     make(map[string]int64),
		kickLocks:    make(map[string]*sync.Mutex),
		lastArchive:  time.Now(),
	}
}

func (e *clientLimitEngine) run() {
	tail := time.NewTicker(clientIPTailInterval)
	defer tail.Stop()
	sweep := time.NewTicker(clientIPSweepInterval)
	defer sweep.Stop()
	for {
		select {
		case <-tail.C:
			// Tail first so everything readable is processed before
			// maintainOnce may archive (and truncate) the log.
			e.tailOnce()
			e.maintainOnce()
		case <-sweep.C:
			e.sweepOnce()
		}
	}
}

// ---------------------------------------------------------------------------
// Phase A: incremental tailer
// ---------------------------------------------------------------------------

// tailOnce reads only the new bytes of the access log since the last call.
// It handles: access log path changes (start at EOF, no replay),
// truncation/rotation (file shrank, or head content replaced), and partial
// trailing lines (buffered until a newline arrives). Duplicate data is
// harmless by construction: sightings are keyed by (email, ip) and re-seeing
// an event only refreshes lastSeen.
func (e *clientLimitEngine) tailOnce() {
	resolve := e.accessPathFn
	if resolve == nil {
		resolve = xray.GetAccessLogPath
	}
	path, err := resolve()
	if err != nil || path == "" || path == "none" {
		return // access logging disabled; engine dormant
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if path != e.accessPath {
		// Access log path changed (or first run): start at EOF, no replay.
		e.accessPath = path
		e.readOffset = 0
		e.partial = nil
		e.fingerprint = nil
		if st, err := os.Stat(path); err == nil {
			e.readOffset = st.Size()
		}
		return
	}

	st, err := os.Stat(path)
	if err != nil {
		return // file missing right now; next tick retries
	}
	size := st.Size()

	// Truncation (our own archive, clear_logs, copytruncate rotation).
	if size < e.readOffset {
		e.readOffset = 0
		e.partial = nil
		e.fingerprint = nil
	}

	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	// Replacement detection: verify the file head still matches what we last
	// consumed. This also covers the same-size replacement case (logrotate
	// "create" with equally sized content) where the size alone looks idle.
	// The check is a 128-byte read; cheap enough for every tick.
	if e.readOffset > 0 {
		head := make([]byte, 128)
		n, _ := f.ReadAt(head, 0)
		head = head[:n]
		if e.fingerprint == nil {
			// We started at EOF and never sampled the head: adopt it.
			e.fingerprint = head
		} else if !bytes.Equal(head, e.fingerprint) {
			e.readOffset = 0
			e.partial = nil
			e.fingerprint = nil
		}
	}

	if size == e.readOffset && len(e.partial) == 0 {
		return // nothing new (head verified above)
	}

	// A detected replacement larger than the catch-up cap is skipped to EOF;
	// re-reading gigabytes of history would stall the engine for nothing.
	if e.readOffset == 0 && size > clientIPCatchUpCap {
		e.readOffset = size
		e.partial = nil
		e.fingerprint = nil
		return
	}

	if size > e.readOffset {
		buf := make([]byte, size-e.readOffset)
		n, err := f.ReadAt(buf, e.readOffset)
		if err != nil && err != io.EOF && n == 0 {
			return
		}
		buf = buf[:n]
		if e.readOffset == 0 {
			m := len(buf)
			if m > 128 {
				m = 128
			}
			e.fingerprint = append([]byte(nil), buf[:m]...)
		}
		e.readOffset += int64(n)
		e.partial = append(e.partial, buf...)
	}

	// Process only complete lines; the trailing fragment stays buffered.
	lastNL := bytes.LastIndexByte(e.partial, '\n')
	if lastNL < 0 {
		return
	}
	complete := e.partial[:lastNL+1]
	e.processLines(complete)
	rest := e.partial[lastNL+1:]
	e.partial = append(e.partial[:0:0], rest...)
}

// processLines handles a run of newline-terminated log lines. Called with
// e.mu held.
func (e *clientLimitEngine) processLines(data []byte) {
	for len(data) > 0 {
		idx := bytes.IndexByte(data, '\n')
		if idx < 0 {
			return
		}
		line := string(data[:idx])
		data = data[idx+1:]
		e.linesProcessed++
		e.observeLine(line)
	}
}

// observeLine extracts (email, source IP, timestamp) from one access-log
// line and records the sighting. Called with e.mu held.
func (e *clientLimitEngine) observeLine(line string) {
	m := clientIPLimitLineRe.FindStringSubmatch(line)
	if m == nil {
		return
	}
	ip := m[1]
	if ip == "127.0.0.1" || ip == "::1" {
		return
	}
	em := clientIPLimitEmailRe.FindStringSubmatch(line)
	if em == nil {
		return
	}
	email := strings.TrimSpace(em[1])
	if email == "" {
		return
	}

	ts := e.now().Unix()
	if tm := clientIPLimitTimeRe.FindStringSubmatch(line); tm != nil {
		if t, err := time.ParseInLocation("2006/01/02 15:04:05", tm[1], time.Local); err == nil {
			ts = t.Unix()
		}
	}

	e.observeEvent(email, ip, ts)
}

// ---------------------------------------------------------------------------
// Phase B: in-memory client/IP state
// ---------------------------------------------------------------------------

// observeEvent records one sighting. Only clients with a positive IP limit
// are tracked; everyone else is filtered by the policy cache. Called with
// e.mu held.
func (e *clientLimitEngine) observeEvent(email, ip string, ts int64) {
	e.eventsObserved++
	policy := e.policyFor(email)
	if policy == nil || policy.limitIP <= 0 || !policy.enable {
		return
	}
	st := e.clients[email]
	if st == nil {
		st = &clientIPState{ips: make(map[string]*clientIPSighting)}
		e.clients[email] = st
	}
	if s := st.ips[ip]; s != nil {
		if ts > s.lastSeen {
			s.lastSeen = ts
		}
		return
	}
	// Bound per-client memory; evict the least recently seen IP first.
	if len(st.ips) >= maxClientIPsTracked {
		var oldestIP string
		var oldest int64
		first := true
		for candIP, s := range st.ips {
			if first || s.lastSeen < oldest {
				oldestIP, oldest, first = candIP, s.lastSeen, false
			}
		}
		delete(st.ips, oldestIP)
	}
	st.ips[ip] = &clientIPSighting{firstSeen: ts, lastSeen: ts}
}

// ---------------------------------------------------------------------------
// Phase C + D: TTL/grace semantics and enforcement
// ---------------------------------------------------------------------------

// maintainOnce expires stale sightings. The legacy standalone engine can
// still enforce grace-based violations when used outside production; the
// production engine only observes recent IPs because the fork gate owns
// admission synchronously.
// Enforcement runs outside the engine lock so tailing never stalls on the
// gRPC round-trips; the run loop is single-goroutine so at most one
// maintain pass is active at any time.
func (e *clientLimitEngine) maintainOnce() {
	if e.gateAuthoritative {
		e.expireObservedIPs()
		e.maybeArchive()
		return
	}

	type kickTask struct {
		email  string
		policy *clientIPPolicy
	}
	var tasks []kickTask

	// Sticky-rule inputs gathered under the lock (design: iplimit.md):
	// kickViolations maps email -> violating IPs for this pass's kicks;
	// liveIPs maps email -> its post-expiry active IP set, used to release
	// rules whose (email, ip) no longer violates anything.
	var kickViolations map[string][]string
	var liveIPs map[string][]string

	e.mu.Lock()
	router := e.limitRouter
	now := e.now()
	for email, st := range e.clients {
		policy := e.policyFor(email)
		if policy == nil || policy.limitIP <= 0 || !policy.enable {
			delete(e.clients, email)
			// Client gone/disabled/limit removed: every sticky rule of
			// this email must be lifted, synchronously is fine (rare).
			if router != nil {
				router.ReleaseAll(email)
			}
			continue
		}

		// An IP is active while its last sighting is within the grace TTL.
		cutoff := now.Add(-time.Duration(policy.graceSec) * time.Second).Unix()
		for ip, s := range st.ips {
			if s.lastSeen < cutoff {
				delete(st.ips, ip)
			}
		}

		if len(st.ips) <= policy.limitIP {
			st.pendingSince = 0
			if len(st.ips) == 0 {
				delete(e.clients, email) // idle client, nothing left to watch
			}
			continue
		}

		// Over the limit: start (or keep) the pending window. A violation is
		// only punished after it persists for the whole grace period.
		if st.pendingSince == 0 {
			st.pendingSince = now.Unix()
			continue
		}
		if now.Unix() < st.pendingSince+int64(policy.graceSec) {
			continue
		}
		// Cooldown (and backoff after failed kicks) between attempts.
		cooldown := int64(policy.graceSec)
		if st.kickFailures > 0 {
			backoff := cooldown * int64(st.kickFailures+1)
			if backoff > cooldown*6 {
				backoff = cooldown * 6
			}
			cooldown = backoff
		}
		if st.lastKick != 0 && now.Unix()-st.lastKick < cooldown {
			continue
		}
		tasks = append(tasks, kickTask{email: email, policy: policy})
	}

	if router != nil {
		kickViolations = make(map[string][]string)
		liveIPs = make(map[string][]string)
		for _, t := range tasks {
			kickViolations[t.email] = e.violatingIPsLocked(t.email, t.policy.limitIP)
		}
		for email, st := range e.clients {
			for ip := range st.ips {
				liveIPs[email] = append(liveIPs[email], ip)
			}
		}
	}
	e.mu.Unlock()

	for _, t := range tasks {
		if err := e.enforce(t.email, t.policy); err != nil {
			e.recordKickFailure(t.email)
			continue
		}
		e.applyPostKick(t.email, t.policy)
	}

	// Sticky enforcement: blackhole rules for this pass's violating IPs.
	// After the kick, applyPostKick keeps only the newest limitIP IPs in
	// state, so the violating IPs no longer appear in liveIPs; the release
	// pass below must therefore NOT treat them as "gone" (they still have
	// live Xray connections to blackhole — that is the entire point).
	if router != nil {
		for email, ips := range kickViolations {
			router.Enforce(email, ips)
		}
		// Release rules that no longer correspond to a violation: the email
		// is tracked, the IP is either no longer active at all, or the email
		// is back at/under its limit.
		for _, snap := range router.Snapshot() {
			// Skip IPs enforced THIS pass: their absence from liveIPs is
			// applyPostKick's bookkeeping, not the IP leaving.
			if kickViolations[snap.Email] != nil && containsStr(kickViolations[snap.Email], snap.IP) {
				continue
			}
			active, tracked := liveIPs[snap.Email]
			if !tracked {
				continue // email state vanished after the lock; next pass
			}
			if containsStr(active, snap.IP) {
				// Still an active IP: keep the rule only while the client
				// is over the limit; under the limit it must be lifted.
				if !e.overLimit(snap.Email) {
					router.Release(snap.Email, snap.IP)
				}
				continue
			}
			// IP no longer active (TTL expiry): lift the rule.
			router.Release(snap.Email, snap.IP)
		}
	}

	e.maybeArchive()
}

// expireObservedIPs keeps only recent log sightings for the UI. Its fixed
// retention window is telemetry cleanup, never an admission grace period.
func (e *clientLimitEngine) expireObservedIPs() {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	cutoff := now.Add(-time.Duration(defaultClientIPGrace) * time.Second).Unix()
	for email, st := range e.clients {
		policy := e.policyFor(email)
		if policy == nil || policy.limitIP <= 0 || !policy.enable {
			delete(e.clients, email)
			if e.limitRouter != nil {
				e.limitRouter.ReleaseAll(email)
			}
			continue
		}
		for ip, sighting := range st.ips {
			if sighting.lastSeen < cutoff {
				delete(st.ips, ip)
			}
		}
		if len(st.ips) == 0 {
			delete(e.clients, email)
		}
	}
}

// enforce serializes the kick per email and delegates to the configured
// kick function (the real gRPC path unless a test replaced it).
func (e *clientLimitEngine) enforce(email string, policy *clientIPPolicy) error {
	e.kickLocksMu.Lock()
	l, ok := e.kickLocks[email]
	if !ok {
		l = &sync.Mutex{}
		e.kickLocks[email] = l
	}
	e.kickLocksMu.Unlock()

	kickFn := e.kickFn
	if kickFn == nil {
		kickFn = e.kickViaXrayAPI
	}
	l.Lock()
	defer l.Unlock()
	return kickFn(email, policy)
}

// kickViaXrayAPI drops a client's live connections by removing and
// re-adding the user through the Xray gRPC API. It never writes to the
// database and re-reads the client's inbound fresh so a client that was
// just disabled, edited or deleted is skipped instead of kicked.
func (e *clientLimitEngine) kickViaXrayAPI(email string, policy *clientIPPolicy) error {
	db := database.GetDB()
	if db == nil {
		return errors.New("database not initialized")
	}

	// Fresh look at the client: the policy cache can be seconds old.
	fresh := e.loadPolicyForInbound(email, policy.inboundId)
	if fresh == nil || fresh.limitIP <= 0 || !fresh.enable {
		return nil // deleted, disabled, or limit removed in the meantime
	}

	inbound := &model.Inbound{}
	if err := db.First(inbound, fresh.inboundId).Error; err != nil {
		return nil // inbound gone; nothing to kick
	}
	settings := map[string]any{}
	if err := json.Unmarshal([]byte(inbound.Settings), &settings); err != nil {
		return nil
	}
	clientsRaw, _ := settings["clients"].([]any)
	var client *model.Client
	for _, c := range clientsRaw {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if cEmail, _ := cm["email"].(string); cEmail == email {
			b, err := json.Marshal(cm)
			if err != nil {
				return nil
			}
			client = &model.Client{}
			if err := json.Unmarshal(b, client); err != nil {
				return nil
			}
			break
		}
	}
	if client == nil {
		return nil // client removed in the meantime
	}

	// Only protocols whose users can be re-added through the handler API.
	protocol := string(inbound.Protocol)
	switch protocol {
	case "vmess", "vless", "trojan", "shadowsocks":
	default:
		logger.Debugf("[LIMIT_IP] Temporary disconnect is not supported for protocol %s on inbound %s", protocol, inbound.Tag)
		return nil
	}

	// Log the excess IPs in the same format the fail2ban filter expects
	// (fail2ban remains optional; this is only a log line).
	e.logExcessIPs(email, fresh)

	// Build the user map the handler API needs. Only the fields Xray itself
	// requires are sent: the model's panel-only metadata (limitIp,
	// limitGrace, enable, disableReason, subId, tgId, ownerUserId, ...)
	// never reaches the Xray runtime config (see GetXrayConfig's strip for
	// the same principle). For shadowsocks the cipher comes from the inbound
	// settings ("method").
	clientConfig := map[string]any{
		"email": client.Email,
	}
	switch protocol {
	case "vmess":
		clientConfig["id"] = client.ID
		clientConfig["security"] = client.Security
	case "vless":
		clientConfig["id"] = client.ID
		clientConfig["flow"] = client.Flow
	case "trojan":
		clientConfig["password"] = client.Password
	case "shadowsocks":
		clientConfig["password"] = client.Password
		if method, ok := settings["method"].(string); ok && method != "" {
			clientConfig["cipher"] = method
		}
	}

	var xrayAPI xray.XrayAPI
	if err := xrayAPI.Init(e.resolveXrayAPIPort()); err != nil {
		return err
	}
	defer xrayAPI.Close()

	// Remove the user to drop all of their connections...
	if err := xrayAPI.RemoveUser(inbound.Tag, email); err != nil {
		return err
	}
	// ...give Xray a moment to close them, then re-add so reconnects work.
	time.Sleep(100 * time.Millisecond)
	return xrayAPI.AddUser(protocol, inbound.Tag, clientConfig)
}

// logExcessIPs writes fail2ban-compatible [LIMIT_IP] lines for the IPs beyond
// the client's limit. Best effort: a logging failure must not block the kick.
func (e *clientLimitEngine) logExcessIPs(email string, policy *clientIPPolicy) {
	excess := e.excessIPs(email, policy.limitIP)
	if len(excess) == 0 {
		return
	}
	logIpFile, err := os.OpenFile(xray.GetIPLimitLogPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		logger.Warningf("[LIMIT_IP] failed to open IP limit log file: %s", err)
		return
	}
	defer logIpFile.Close()
	l := log.New(logIpFile, "", log.LstdFlags)
	for _, ipTs := range excess {
		l.Printf("[LIMIT_IP] Email = %s || Disconnecting OLD IP = %s || Timestamp = %d", email, ipTs.IP, ipTs.Timestamp)
	}
}

// sightingNewer orders sightings newest-first with a deterministic
// tie-break. Access-log timestamps have second resolution, so two IPs of one
// client commonly share a lastSeen, and sort.Slice is unstable: excessIPs,
// violatingIPsLocked and applyPostKick each sort the same state map and MUST
// agree on the order — the sticky router blackholes the tail of the list
// while applyPostKick keeps its head, so a disagreement would blackhole a
// kept IP. The IP-string tie-break is arbitrary but stable.
func sightingNewer(a, b ClientIPWithTimestamp) bool {
	if a.Timestamp != b.Timestamp {
		return a.Timestamp > b.Timestamp
	}
	return a.IP > b.IP
}

// excessIPs returns the client's active IPs beyond the limit, most recently
// seen first. Called without e.mu held.
func (e *clientLimitEngine) excessIPs(email string, limitIP int) []ClientIPWithTimestamp {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.clients[email]
	if st == nil || len(st.ips) <= limitIP {
		return nil
	}
	all := make([]ClientIPWithTimestamp, 0, len(st.ips))
	for ip, s := range st.ips {
		all = append(all, ClientIPWithTimestamp{IP: ip, Timestamp: s.lastSeen})
	}
	sort.Slice(all, func(i, j int) bool { return sightingNewer(all[i], all[j]) })
	return all[limitIP:]
}

// violatingIPsLocked returns just the IP strings beyond the client's limit,
// most recently seen first. Called with e.mu held (maintainOnce's sticky
// enforcement pass); the router-side equivalent of excessIPs.
func (e *clientLimitEngine) violatingIPsLocked(email string, limitIP int) []string {
	st := e.clients[email]
	if st == nil || len(st.ips) <= limitIP {
		return nil
	}
	all := make([]ClientIPWithTimestamp, 0, len(st.ips))
	for ip, s := range st.ips {
		all = append(all, ClientIPWithTimestamp{IP: ip, Timestamp: s.lastSeen})
	}
	sort.Slice(all, func(i, j int) bool { return sightingNewer(all[i], all[j]) })
	ips := make([]string, 0, len(all)-limitIP)
	for _, v := range all[limitIP:] {
		ips = append(ips, v.IP)
	}
	return ips
}

// overLimit reports whether the client is currently above its IP limit.
// Lock-free on purpose: called from the release pass after e.mu is freed,
// so it takes the lock itself.
func (e *clientLimitEngine) overLimit(email string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.clients[email]
	if st == nil {
		return false
	}
	policy := e.policyFor(email)
	return policy != nil && policy.limitIP > 0 && len(st.ips) > policy.limitIP
}

// containsStr reports whether the slice holds s.
func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// applyPostKick updates the client state after a successful kick: the most
// recently seen IPs up to the limit are kept (they are the surviving
// legitimate connections), the excess is forgotten, and the cooldown clock
// starts.
func (e *clientLimitEngine) applyPostKick(email string, policy *clientIPPolicy) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.clients[email]
	if st == nil {
		return
	}
	now := e.now().Unix()
	st.lastKick = now
	st.pendingSince = 0
	st.kickFailures = 0
	if len(st.ips) > policy.limitIP {
		all := make([]ClientIPWithTimestamp, 0, len(st.ips))
		for ip, s := range st.ips {
			all = append(all, ClientIPWithTimestamp{IP: ip, Timestamp: s.lastSeen})
		}
		sort.Slice(all, func(i, j int) bool { return sightingNewer(all[i], all[j]) })
		keepSet := make(map[string]struct{}, policy.limitIP)
		for _, k := range all[:policy.limitIP] {
			keepSet[k.IP] = struct{}{}
		}
		for ip := range st.ips {
			if _, ok := keepSet[ip]; !ok {
				delete(st.ips, ip)
			}
		}
	}
	e.kicksExecuted++
}

// recordKickFailure backs off after a failed enforcement attempt (e.g. the
// Xray API is unavailable); the pending violation is retried after the
// backoff window.
func (e *clientLimitEngine) recordKickFailure(email string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if st := e.clients[email]; st != nil {
		st.lastKick = e.now().Unix()
		st.kickFailures++
	}
	e.kickFailures++
}

// ---------------------------------------------------------------------------
// Phase E: policy cache
// ---------------------------------------------------------------------------

// policyFor returns the cached policy for an email, loading it from the
// database on cache miss or expiry. Unknown emails get a short-lived
// negative entry so untracked emails cost no queries. Called with e.mu held.
func (e *clientLimitEngine) policyFor(email string) *clientIPPolicy {
	now := e.now()
	if p, ok := e.policy[email]; ok {
		if now.Sub(time.Unix(p.loadedAt, 0)) < clientIPCacheTTL {
			return p
		}
		delete(e.policy, email)
	}
	if exp, ok := e.negative[email]; ok {
		if now.Unix() < exp {
			return nil
		}
		delete(e.negative, email)
	}
	p := e.loadPolicy(email)
	if p == nil {
		e.negative[email] = now.Add(clientIPNegativeCacheTTL).Unix()
		return nil
	}
	p.loadedAt = now.Unix()
	e.policy[email] = p
	return p
}

// loadPolicy resolves a client's limit policy through two indexed lookups:
// client_traffics.email -> inbound_id, then the inbound's settings clients.
// No LIKE scan, no full-inbound load.
func (e *clientLimitEngine) loadPolicy(email string) *clientIPPolicy {
	db := database.GetDB()
	if db == nil {
		return nil
	}
	var traffic xray.ClientTraffic
	if err := db.Where("email = ?", email).First(&traffic).Error; err != nil {
		return nil
	}
	return e.loadPolicyForInbound(email, traffic.InboundId)
}

func (e *clientLimitEngine) loadPolicyForInbound(email string, inboundId int) *clientIPPolicy {
	db := database.GetDB()
	if db == nil {
		return nil
	}
	inbound := &model.Inbound{}
	if err := db.First(inbound, inboundId).Error; err != nil {
		return nil
	}
	// Real inbound settings carry extra top-level keys alongside "clients"
	// ("decryption" for vless, "disableInsecureEncryption" for vmess, ...).
	// Unmarshalling into map[string][]model.Client returns an
	// UnmarshalTypeError for those keys even though "clients" itself parses
	// fine — which made this lookup return nil for EVERY real vless/vmess
	// inbound, so the engine silently never enforced any limit. A struct
	// ignores unknown keys, so only a genuinely malformed settings blob fails.
	var settings struct {
		Clients []model.Client `json:"clients"`
	}
	if err := json.Unmarshal([]byte(inbound.Settings), &settings); err != nil {
		return nil
	}
	for _, c := range settings.Clients {
		if c.Email != email {
			continue
		}
		grace := c.LimitGrace
		if grace <= 0 {
			grace = defaultClientIPGrace
		} else if grace < minClientIPGrace {
			grace = minClientIPGrace
		}
		return &clientIPPolicy{
			inboundId: inbound.Id,
			tag:       inbound.Tag,
			protocol:  string(inbound.Protocol),
			limitIP:   c.LimitIP,
			graceSec:  grace,
			enable:    c.Enable && inbound.Enable,
			loadedAt:  e.now().Unix(),
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Access log archival (replaces the old job's hourly clear)
// ---------------------------------------------------------------------------

// maybeArchive copies the access log to the persistent log and truncates it
// when it grew past the size cap or holds unarchived content older than an
// hour. The tailer picks the truncation up as a rotation on the next tick,
// so no event is processed twice and none is lost (tailOnce always runs
// before maintainOnce in the run loop).
func (e *clientLimitEngine) maybeArchive() {
	e.mu.Lock()
	path := e.accessPath
	if path == "" {
		e.mu.Unlock()
		return
	}
	st, err := os.Stat(path)
	if err != nil || st.Size() == 0 {
		e.lastArchive = e.now()
		e.mu.Unlock()
		return
	}
	size := st.Size()
	now := e.now()
	if size <= clientIPArchiveSize && now.Sub(e.lastArchive) < clientIPArchiveInterval {
		e.mu.Unlock()
		return
	}
	e.lastArchive = now
	e.mu.Unlock()

	persistent, err := os.OpenFile(xray.GetAccessPersistentLogPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		logger.Warningf("[LIMIT_IP] failed to open persistent access log: %s", err)
		return
	}
	defer persistent.Close()
	file, err := os.Open(path)
	if err != nil {
		logger.Warningf("[LIMIT_IP] failed to open access log for archival: %s", err)
		return
	}
	defer file.Close()
	if _, err := io.Copy(persistent, file); err != nil {
		logger.Warningf("[LIMIT_IP] failed to archive access log: %s", err)
		return
	}
	if err := os.Truncate(path, 0); err != nil {
		logger.Warningf("[LIMIT_IP] failed to truncate access log: %s", err)
	}
}

// ---------------------------------------------------------------------------
// Periodic sweep
// ---------------------------------------------------------------------------

// sweepOnce prunes cache and state entries for clients that went quiet and
// enforcement locks for emails no longer tracked, so the in-memory maps
// stay proportional to active clients, not to the panel's lifetime.
func (e *clientLimitEngine) sweepOnce() {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	for email, p := range e.policy {
		if now.Sub(time.Unix(p.loadedAt, 0)) > 2*clientIPCacheTTL {
			delete(e.policy, email)
		}
	}
	for email, exp := range e.negative {
		if now.Unix() >= exp {
			delete(e.negative, email)
		}
	}
	for email, st := range e.clients {
		if len(st.ips) == 0 && st.pendingSince == 0 {
			delete(e.clients, email)
		}
	}
	e.kickLocksMu.Lock()
	for email := range e.kickLocks {
		if _, ok := e.clients[email]; !ok {
			delete(e.kickLocks, email)
		}
	}
	e.kickLocksMu.Unlock()
}

// ---------------------------------------------------------------------------
// Xray API port resolution (moved from web/job/check_client_ip_job.go)
// ---------------------------------------------------------------------------

// resolveXrayAPIPort returns the API inbound port from the running Xray
// process when available, then the config file, then the DB template, then
// the default.
func (e *clientLimitEngine) resolveXrayAPIPort() int {
	if e.apiPort > 0 {
		return e.apiPort
	}
	if p != nil {
		if port := p.GetAPIPort(); port > 0 {
			return port
		}
	}
	if port, err := getAPIPortFromConfigPath(xray.GetConfigPath()); err == nil {
		return port
	}
	db := database.GetDB()
	if db != nil {
		var template model.Setting
		if err := db.Where("key = ?", "xrayTemplateConfig").First(&template).Error; err == nil {
			if port, parseErr := getAPIPortFromConfigData([]byte(template.Value)); parseErr == nil {
				return port
			}
		}
	}
	return defaultXrayAPIPort
}

func getAPIPortFromConfigPath(configPath string) (int, error) {
	configData, err := os.ReadFile(configPath)
	if err != nil {
		return 0, err
	}
	return getAPIPortFromConfigData(configData)
}

func getAPIPortFromConfigData(configData []byte) (int, error) {
	xrayConfig := &xray.Config{}
	if err := json.Unmarshal(configData, xrayConfig); err != nil {
		return 0, err
	}
	for _, inboundConfig := range xrayConfig.InboundConfigs {
		if inboundConfig.Tag == "api" && inboundConfig.Port > 0 {
			return inboundConfig.Port, nil
		}
	}
	return 0, errors.New("api inbound port not found")
}

// ---------------------------------------------------------------------------
// Live state accessors and invalidation hooks (used by InboundService)
// ---------------------------------------------------------------------------

// clientLimitEngineLiveIPs returns the engine's live view of a client's
// active IPs (most recent first), or nil when the engine has never seen the
// client. Engine state is memory-only; the inbound_client_ips table is not
// written per event anymore.
func clientLimitEngineLiveIPs(email string) []ClientIPWithTimestamp {
	e := clientLimitEngineInst
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.clients[email]
	if st == nil {
		return nil
	}
	ips := make([]ClientIPWithTimestamp, 0, len(st.ips))
	for ip, s := range st.ips {
		ips = append(ips, ClientIPWithTimestamp{IP: ip, Timestamp: s.lastSeen})
	}
	sort.Slice(ips, func(i, j int) bool { return sightingNewer(ips[i], ips[j]) })
	return ips
}

// clientLimitEngineReset forgets a client's live state and cached policy
// (used by ClearClientIps so the log-clear button takes effect at once).
// Sticky rules of the client are released: the operator-visible state and
// the Xray-side enforcement must agree.
func clientLimitEngineReset(email string) {
	e := clientLimitEngineInst
	if e == nil {
		return
	}
	e.mu.Lock()
	delete(e.clients, email)
	delete(e.policy, email)
	delete(e.negative, email)
	router := e.limitRouter
	e.mu.Unlock()
	if router != nil {
		router.ReleaseAll(email)
	}
}

// clientLimitEngineForgetClient drops every trace of a client (used when the
// client or its IP bindings are deleted).
func clientLimitEngineForgetClient(email string) {
	clientLimitEngineReset(email)
}

// clientLimitEngineNotifyClientUpdated invalidates the cached policy after a
// client was created, edited or enabled/disabled. When the email changed,
// live state (active IPs, pending violation) moves to the new email so an
// edit does not reset or fabricate IP history.
func clientLimitEngineNotifyClientUpdated(oldEmail, newEmail string) {
	// Invalidate observation state here. Public DB mutation methods refresh
	// the fork policy after their transaction commits, so this helper never
	// publishes an intermediate snapshot from inside a transaction.
	e := clientLimitEngineInst
	if e == nil {
		return
	}
	var releaseEmails []string
	e.mu.Lock()
	router := e.limitRouter
	if oldEmail != "" {
		delete(e.policy, oldEmail)
		delete(e.negative, oldEmail)
	}
	if newEmail != "" && newEmail != oldEmail {
		delete(e.policy, newEmail)
		delete(e.negative, newEmail)
	}
	if oldEmail != "" && newEmail != "" && oldEmail != newEmail {
		if st, ok := e.clients[oldEmail]; ok {
			e.clients[newEmail] = st
			delete(e.clients, oldEmail)
		}
		// Rules were keyed by the OLD email; they must be re-armed under
		// the new one. Release by old email; the next violation re-arms.
		releaseEmails = append(releaseEmails, oldEmail)
	} else if oldEmail != "" && newEmail == "" {
		// Disabled or deleted: lift all sticky rules at once.
		delete(e.clients, oldEmail)
		releaseEmails = append(releaseEmails, oldEmail)
	}
	e.mu.Unlock()

	if router != nil {
		for _, email := range releaseEmails {
			router.ReleaseAll(email)
		}
	}
}

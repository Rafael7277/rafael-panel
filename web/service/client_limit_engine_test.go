package service

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/xray"
)

// --- test helpers -----------------------------------------------------------

// fakeClock gives tests deterministic control over the engine's notion of
// "now" (grace windows, pending deadlines, cooldowns).
type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 9, 7, 12, 0, 0, 0, time.Local)}
}

// newLimitEngineForTest builds an engine wired to a temp access log, an
// injectable clock and kick function, and an initialized test database. The
// access log path is registered up front (production engines start at EOF;
// tests need every appended line processed).
func newLimitEngineForTest(t *testing.T, clock *fakeClock, kickFn func(email string, policy *clientIPPolicy) error) (*clientLimitEngine, string) {
	t.Helper()
	initLifecycleTestDB(t)
	accessPath := filepath.Join(t.TempDir(), "access.log")
	writeAccessLog(t, accessPath, "")
	e := newClientLimitEngine()
	e.now = clock.Now
	e.kickFn = kickFn
	e.lastArchive = clock.Now()
	e.accessPathFn = func() (string, error) { return accessPath, nil }
	e.tailOnce() // register the path
	return e, accessPath
}

func writeAccessLog(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write access log: %v", err)
	}
}

func appendAccessLog(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open access log: %v", err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("append access log: %v", err)
	}
}

// limitLine formats one access-log line in the shape the tailer's regexes
// expect, timestamped at the given (fake) time.
func limitLine(ts time.Time, ip, email string) string {
	return fmt.Sprintf("%s from tcp:%s:54321 accepted email: %s\n", ts.Format("2006/01/02 15:04:05"), ip, email)
}

// createLimitInbound creates one enabled vmess inbound with the given clients
// (settings JSON + client_traffics rows), so the engine can resolve policy.
func createLimitInbound(t *testing.T, clients []model.Client) *model.Inbound {
	t.Helper()
	db := database.GetDB()
	settings := map[string]any{"clients": clients}
	body, err := json.Marshal(settings)
	if err != nil {
		t.Fatalf("marshal settings: %v", err)
	}
	inbound := &model.Inbound{
		UserId:   1,
		Tag:      "limit-test-in",
		Enable:   true,
		Port:     10000,
		Protocol: model.VMESS,
		Settings: string(body),
	}
	if err := db.Create(inbound).Error; err != nil {
		t.Fatalf("create inbound: %v", err)
	}
	for i := range clients {
		ct := xray.ClientTraffic{InboundId: inbound.Id, Email: clients[i].Email, Enable: clients[i].Enable}
		if err := db.Create(&ct).Error; err != nil {
			t.Fatalf("create client traffic for %s: %v", clients[i].Email, err)
		}
	}
	return inbound
}

// reloadInboundSettings re-reads the inbound's settings blob from the DB.
func reloadInboundSettings(t *testing.T, inboundId int) string {
	t.Helper()
	var inbound model.Inbound
	if err := database.GetDB().First(&inbound, inboundId).Error; err != nil {
		t.Fatalf("reload inbound: %v", err)
	}
	return inbound.Settings
}

func trackedIPs(e *clientLimitEngine, email string) map[string]*clientIPSighting {
	e.mu.Lock()
	defer e.mu.Unlock()
	if st := e.clients[email]; st != nil {
		return st.ips
	}
	return nil
}

// --- 1. single IP under the limit -------------------------------------------

func TestClientLimitSingleIPUnderLimitNoKick(t *testing.T) {
	clock := newFakeClock()
	var kicks atomic.Int32
	e, path := newLimitEngineForTest(t, clock, func(email string, p *clientIPPolicy) error {
		kicks.Add(1)
		return nil
	})
	createLimitInbound(t, []model.Client{{ID: "uuid-1", Email: "user@test", LimitIP: 1, Enable: true}})

	// Continuous traffic from a single IP must never trigger a kick.
	for i := 0; i < 5; i++ {
		appendAccessLog(t, path, limitLine(clock.Now(), "1.2.3.4", "user@test"))
		e.tailOnce()
		e.maintainOnce()
		clock.Advance(30 * time.Second)
	}
	if got := kicks.Load(); got != 0 {
		t.Fatalf("kicks = %d, want 0", got)
	}
}

// --- 2. second IP: grace period before enforcement ---------------------------

func TestClientLimitSecondIPGraceBeforeKick(t *testing.T) {
	clock := newFakeClock()
	var kicks atomic.Int32
	e, path := newLimitEngineForTest(t, clock, func(email string, p *clientIPPolicy) error {
		kicks.Add(1)
		return nil
	})
	createLimitInbound(t, []model.Client{{ID: "uuid-1", Email: "user@test", LimitIP: 1, Enable: true}})

	// First IP connects.
	appendAccessLog(t, path, limitLine(clock.Now(), "1.1.1.1", "user@test"))
	e.tailOnce()
	e.maintainOnce()

	// A second distinct IP appears while the first stays active: this is a
	// violation, but enforcement waits out the grace period.
	appendAccessLog(t, path, limitLine(clock.Now(), "2.2.2.2", "user@test"))
	e.tailOnce()
	e.maintainOnce()
	if got := kicks.Load(); got != 0 {
		t.Fatalf("kicked immediately, want grace window first")
	}

	// Both IPs keep reconnecting so they stay active, but we stay inside the
	// grace window: still no kick.
	clock.Advance(30 * time.Second)
	appendAccessLog(t, path, limitLine(clock.Now(), "1.1.1.1", "user@test")+limitLine(clock.Now(), "2.2.2.2", "user@test"))
	e.tailOnce()
	e.maintainOnce()
	if got := kicks.Load(); got != 0 {
		t.Fatalf("kicks = %d during grace window, want 0", got)
	}
}

// --- 3. mobile-network IP change must not accumulate -------------------------

func TestClientLimitMobileIPChangeNoKick(t *testing.T) {
	clock := newFakeClock()
	var kicks atomic.Int32
	e, path := newLimitEngineForTest(t, clock, func(email string, p *clientIPPolicy) error {
		kicks.Add(1)
		return nil
	})
	createLimitInbound(t, []model.Client{{ID: "uuid-1", Email: "user@test", LimitIP: 1, Enable: true}})

	// Device connects from IP A for a while, then the mobile network switches
	// it to IP B. A is never seen again.
	for i := 0; i < 3; i++ {
		appendAccessLog(t, path, limitLine(clock.Now(), "10.0.0.1", "user@test"))
		e.tailOnce()
		e.maintainOnce()
		clock.Advance(10 * time.Second)
	}
	for i := 0; i < 5; i++ {
		appendAccessLog(t, path, limitLine(clock.Now(), "10.0.0.2", "user@test"))
		e.tailOnce()
		e.maintainOnce()
		clock.Advance(30 * time.Second)
	}

	// Long after A's grace window expired, only B remains active.
	clock.Advance(5 * time.Minute)
	appendAccessLog(t, path, limitLine(clock.Now(), "10.0.0.2", "user@test"))
	e.tailOnce()
	e.maintainOnce()

	ips := trackedIPs(e, "user@test")
	if ips == nil || len(ips) != 1 {
		t.Fatalf("tracked IPs = %v, want exactly the new IP", ips)
	}
	if _, ok := ips["10.0.0.1"]; ok {
		t.Fatalf("old mobile IP never expired: %v", ips)
	}
	if got := kicks.Load(); got != 0 {
		t.Fatalf("kicks = %d, want 0 (temporary IP change must not ban)", got)
	}
}

// --- 4. simultaneous IPs past the grace period get kicked --------------------

func TestClientLimitSimultaneousIPsKickedAfterGrace(t *testing.T) {
	clock := newFakeClock()
	var kickedEmails []string
	e, path := newLimitEngineForTest(t, clock, func(email string, p *clientIPPolicy) error {
		kickedEmails = append(kickedEmails, email)
		return nil
	})
	createLimitInbound(t, []model.Client{{ID: "uuid-1", Email: "user@test", LimitIP: 1, Enable: true}})

	// Two IPs used simultaneously, both continuously active.
	appendAccessLog(t, path, limitLine(clock.Now(), "1.1.1.1", "user@test")+limitLine(clock.Now(), "2.2.2.2", "user@test"))
	e.tailOnce()
	e.maintainOnce()

	// Keep both alive past the 60s grace window.
	for i := 0; i < 3; i++ {
		clock.Advance(25 * time.Second)
		appendAccessLog(t, path, limitLine(clock.Now(), "1.1.1.1", "user@test")+limitLine(clock.Now(), "2.2.2.2", "user@test"))
		e.tailOnce()
		e.maintainOnce()
	}

	if len(kickedEmails) != 1 || kickedEmails[0] != "user@test" {
		t.Fatalf("kicked = %v, want one kick for user@test", kickedEmails)
	}
	// After the kick only the most recently seen IP is retained.
	if ips := trackedIPs(e, "user@test"); len(ips) != 1 {
		t.Fatalf("post-kick tracked IPs = %v, want 1", ips)
	}
	e.mu.Lock()
	pendingZero := e.clients["user@test"].pendingSince == 0
	e.mu.Unlock()
	if !pendingZero {
		t.Fatalf("pending violation not cleared after kick")
	}
}

// --- 5. duplicate log lines are idempotent -----------------------------------

// Real inbound settings carry extra top-level keys alongside "clients"
// ("decryption" for vless, "disableInsecureEncryption" for vmess). The policy
// lookup must tolerate them: unmarshalling into map[string][]model.Client
// returns an UnmarshalTypeError for those keys, which used to make the engine
// treat every real vless/vmess inbound as "no policy" and never enforce.
func TestClientLimitPolicyLoadsWithExtraSettingsKeys(t *testing.T) {
	clock := newFakeClock()
	var kicks atomic.Int32
	e, path := newLimitEngineForTest(t, clock, func(email string, p *clientIPPolicy) error {
		kicks.Add(1)
		return nil
	})

	// Build settings exactly like a real vless inbound: clients + decryption.
	clients := []model.Client{{ID: "uuid-1", Email: "user@test", LimitIP: 1, Enable: true}}
	settings := map[string]any{"clients": clients, "decryption": "none"}
	body, err := json.Marshal(settings)
	if err != nil {
		t.Fatalf("marshal settings: %v", err)
	}
	db := database.GetDB()
	inbound := &model.Inbound{
		UserId:   1,
		Tag:      "limit-vless-in",
		Enable:   true,
		Port:     10001,
		Protocol: model.VLESS,
		Settings: string(body),
	}
	if err := db.Create(inbound).Error; err != nil {
		t.Fatalf("create inbound: %v", err)
	}
	ct := xray.ClientTraffic{InboundId: inbound.Id, Email: "user@test", Enable: true}
	if err := db.Create(&ct).Error; err != nil {
		t.Fatalf("create client traffic: %v", err)
	}

	// Two simultaneous IPs, both continuously active past the grace window.
	appendAccessLog(t, path, limitLine(clock.Now(), "1.1.1.1", "user@test")+limitLine(clock.Now(), "2.2.2.2", "user@test"))
	e.tailOnce()
	e.maintainOnce()
	for i := 0; i < 3; i++ {
		clock.Advance(25 * time.Second)
		appendAccessLog(t, path, limitLine(clock.Now(), "1.1.1.1", "user@test")+limitLine(clock.Now(), "2.2.2.2", "user@test"))
		e.tailOnce()
		e.maintainOnce()
	}

	if got := kicks.Load(); got != 1 {
		t.Fatalf("kicks = %d with extra settings keys, want 1 (policy must load)", got)
	}
}

func TestClientLimitDuplicateLinesResilience(t *testing.T) {
	clock := newFakeClock()
	var kicks atomic.Int32
	e, path := newLimitEngineForTest(t, clock, func(email string, p *clientIPPolicy) error {
		kicks.Add(1)
		return nil
	})
	createLimitInbound(t, []model.Client{{ID: "uuid-1", Email: "user@test", LimitIP: 1, Enable: true}})

	// The same event line delivered twice (log replay, duplicate writes).
	line := limitLine(clock.Now(), "1.1.1.1", "user@test")
	appendAccessLog(t, path, line+line)
	e.tailOnce()
	if ips := trackedIPs(e, "user@test"); len(ips) != 1 {
		t.Fatalf("tracked IPs after duplicate lines = %v, want 1", ips)
	}

	// Tailing again with no new bytes must not reprocess anything.
	e.tailOnce()
	e.maintainOnce()
	if ips := trackedIPs(e, "user@test"); len(ips) != 1 {
		t.Fatalf("tracked IPs after idle re-tail = %v, want 1", ips)
	}
	if got := kicks.Load(); got != 0 {
		t.Fatalf("kicks = %d, want 0", got)
	}
}

// --- 6. log rotation / replacement -------------------------------------------

func TestClientLimitLogRotationHandled(t *testing.T) {
	clock := newFakeClock()
	var kicks atomic.Int32
	e, path := newLimitEngineForTest(t, clock, func(email string, p *clientIPPolicy) error {
		kicks.Add(1)
		return nil
	})
	createLimitInbound(t, []model.Client{{ID: "uuid-1", Email: "user@test", LimitIP: 1, Enable: true}})

	appendAccessLog(t, path, limitLine(clock.Now(), "1.1.1.1", "user@test"))
	e.tailOnce()
	e.mu.Lock()
	seen := e.eventsObserved
	e.mu.Unlock()

	// Case 1: truncation (archive / clear_logs style) then new content.
	writeAccessLog(t, path, limitLine(clock.Now(), "2.2.2.2", "user@test"))
	e.tailOnce()
	e.mu.Lock()
	afterTruncate := e.eventsObserved
	e.mu.Unlock()
	if afterTruncate-seen != 1 {
		t.Fatalf("after truncate: processed %d events, want exactly the 1 new one (no replay)", afterTruncate-seen)
	}

	// Case 2: replacement with different head content at a size that does not
	// shrink (logrotate create style) — detected via the head fingerprint.
	seen = afterTruncate
	replacement := limitLine(clock.Now(), "3.3.3.3", "user@test") +
		limitLine(clock.Now(), "3.3.3.3", "user@test") +
		limitLine(clock.Now(), "3.3.3.3", "user@test")
	writeAccessLog(t, path, replacement)
	e.tailOnce()
	e.mu.Lock()
	afterReplace := e.eventsObserved
	e.mu.Unlock()
	if afterReplace-seen != 3 {
		t.Fatalf("after replacement: processed %d events, want exactly the 3 new ones (no replay)", afterReplace-seen)
	}
	if got := kicks.Load(); got != 0 {
		t.Fatalf("kicks = %d, want 0 (rotation alone must not kick)", got)
	}
}

// --- 7. partial trailing line is buffered ------------------------------------

func TestClientLimitPartialLineBuffered(t *testing.T) {
	clock := newFakeClock()
	e, path := newLimitEngineForTest(t, clock, nil)
	createLimitInbound(t, []model.Client{{ID: "uuid-1", Email: "user@test", LimitIP: 1, Enable: true}})

	full := limitLine(clock.Now(), "1.1.1.1", "user@test")

	// Only a prefix of the line has been flushed to disk.
	appendAccessLog(t, path, full[:len(full)-10])
	e.tailOnce()
	if ips := trackedIPs(e, "user@test"); ips != nil {
		t.Fatalf("partial line was processed: %v", ips)
	}

	// The rest of the line arrives; now it must be counted exactly once.
	appendAccessLog(t, path, full[len(full)-10:])
	e.tailOnce()
	ips := trackedIPs(e, "user@test")
	if ips == nil || len(ips) != 1 {
		t.Fatalf("tracked IPs = %v, want 1 after line completed", ips)
	}
	if _, ok := ips["1.1.1.1"]; !ok {
		t.Fatalf("IP 1.1.1.1 missing: %v", ips)
	}
}

// --- 8. Xray API unavailable: graceful failure and backoff --------------------

func TestClientLimitXrayAPIUnavailableBackoff(t *testing.T) {
	clock := newFakeClock()
	// Real gRPC kick path, pointed at a port nothing listens on.
	e, path := newLimitEngineForTest(t, clock, nil)

	// Grab a free port and close the listener so it is (almost certainly)
	// unbound; the engine must fail the kick gracefully.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	e.apiPort = l.Addr().(*net.TCPAddr).Port
	l.Close()

	createLimitInbound(t, []model.Client{{ID: "uuid-1", Email: "user@test", LimitIP: 1, Enable: true}})

	appendAccessLog(t, path, limitLine(clock.Now(), "1.1.1.1", "user@test")+limitLine(clock.Now(), "2.2.2.2", "user@test"))
	e.tailOnce()
	e.maintainOnce()

	// Ride out the grace window with both IPs staying active.
	for i := 0; i < 4; i++ {
		clock.Advance(25 * time.Second)
		appendAccessLog(t, path, limitLine(clock.Now(), "1.1.1.1", "user@test")+limitLine(clock.Now(), "2.2.2.2", "user@test"))
		e.tailOnce()
		e.maintainOnce()
	}

	e.mu.Lock()
	failures := e.kickFailures
	executed := e.kicksExecuted
	e.mu.Unlock()
	if failures == 0 {
		t.Fatalf("expected a failed kick attempt against the unavailable API")
	}
	if executed != 0 {
		t.Fatalf("kicksExecuted = %d, want 0 (failed kicks must not count as enforced)", executed)
	}

	// The violation is retried only after the backoff window (grace*2).
	clock.Advance(121 * time.Second)
	appendAccessLog(t, path, limitLine(clock.Now(), "1.1.1.1", "user@test")+limitLine(clock.Now(), "2.2.2.2", "user@test"))
	e.tailOnce()
	e.maintainOnce()
	e.mu.Lock()
	failures2 := e.kickFailures
	e.mu.Unlock()
	if failures2 <= failures {
		t.Fatalf("kick attempts = %d, want more than %d after backoff elapsed", failures2, failures)
	}
}

// --- 9. disabled clients are never kicked ------------------------------------

func TestClientLimitDisabledClientNotKicked(t *testing.T) {
	clock := newFakeClock()
	var kicks atomic.Int32
	e, path := newLimitEngineForTest(t, clock, func(email string, p *clientIPPolicy) error {
		kicks.Add(1)
		return nil
	})
	createLimitInbound(t, []model.Client{
		{ID: "uuid-1", Email: "disabled@test", LimitIP: 1, Enable: false, DisableReason: DisableReasonQuota},
		{ID: "uuid-2", Email: "enabled@test", LimitIP: 0, Enable: true},
	})

	// Traffic from a disabled client and from a client without an IP limit:
	// neither may be tracked or kicked.
	for i := 0; i < 3; i++ {
		appendAccessLog(t, path,
			limitLine(clock.Now(), "1.1.1.1", "disabled@test")+
				limitLine(clock.Now(), "2.2.2.2", "enabled@test"))
		e.tailOnce()
		e.maintainOnce()
		clock.Advance(2 * time.Minute)
	}

	e.mu.Lock()
	tracked := len(e.clients)
	e.mu.Unlock()
	if tracked != 0 {
		t.Fatalf("tracked clients = %d, want 0", tracked)
	}
	if got := kicks.Load(); got != 0 {
		t.Fatalf("kicks = %d, want 0", got)
	}
}

// --- 10. reseller ownership survives enforcement ------------------------------

func TestClientLimitResellerOwnedClientUntouched(t *testing.T) {
	clock := newFakeClock()
	var kicks atomic.Int32
	e, path := newLimitEngineForTest(t, clock, func(email string, p *clientIPPolicy) error {
		kicks.Add(1)
		return nil
	})
	ownerID := 42
	inbound := createLimitInbound(t, []model.Client{
		{ID: "uuid-1", Email: "resold@test", LimitIP: 1, Enable: true, OwnerUserID: ownerID},
	})
	settingsBefore := reloadInboundSettings(t, inbound.Id)

	// Force a persistent violation and let it be enforced.
	appendAccessLog(t, path, limitLine(clock.Now(), "1.1.1.1", "resold@test")+limitLine(clock.Now(), "2.2.2.2", "resold@test"))
	e.tailOnce()
	e.maintainOnce()
	for i := 0; i < 3; i++ {
		clock.Advance(25 * time.Second)
		appendAccessLog(t, path, limitLine(clock.Now(), "1.1.1.1", "resold@test")+limitLine(clock.Now(), "2.2.2.2", "resold@test"))
		e.tailOnce()
		e.maintainOnce()
	}

	if got := kicks.Load(); got != 1 {
		t.Fatalf("kicks = %d, want 1", got)
	}

	// The kick is gRPC-only: the inbound settings blob (ownership, enable
	// flag, disable reason) must be byte-identical, and no IP rows written.
	settingsAfter := reloadInboundSettings(t, inbound.Id)
	if settingsAfter != settingsBefore {
		t.Fatalf("inbound settings changed by enforcement:\nbefore: %s\nafter:  %s", settingsBefore, settingsAfter)
	}
	var count int64
	database.GetDB().Model(model.InboundClientIps{}).Where("client_email = ?", "resold@test").Count(&count)
	if count != 0 {
		t.Fatalf("inbound_client_ips rows = %d, want 0 (no per-event DB writes)", count)
	}

	clients, err := (&InboundService{}).GetClients(inbound)
	if err != nil {
		t.Fatalf("GetClients: %v", err)
	}
	if len(clients) != 1 || clients[0].OwnerUserID != ownerID || !clients[0].Enable || clients[0].DisableReason != "" {
		t.Fatalf("client state changed by enforcement: %+v", clients[0])
	}
}

// --- 11. no fail2ban dependency (Windows parity) ------------------------------

func TestClientLimitNoFail2banDependency(t *testing.T) {
	// The engine enforces through the Xray gRPC API only. This test runs on
	// every platform (including Windows, where fail2ban does not exist) and
	// proves a kick completes with nothing but the injectable kick path.
	clock := newFakeClock()
	var kicks atomic.Int32
	e, path := newLimitEngineForTest(t, clock, func(email string, p *clientIPPolicy) error {
		kicks.Add(1)
		return nil
	})
	createLimitInbound(t, []model.Client{{ID: "uuid-1", Email: "user@test", LimitIP: 1, Enable: true}})

	appendAccessLog(t, path, limitLine(clock.Now(), "1.1.1.1", "user@test")+limitLine(clock.Now(), "2.2.2.2", "user@test"))
	e.tailOnce()
	e.maintainOnce()
	for i := 0; i < 3; i++ {
		clock.Advance(25 * time.Second)
		appendAccessLog(t, path, limitLine(clock.Now(), "1.1.1.1", "user@test")+limitLine(clock.Now(), "2.2.2.2", "user@test"))
		e.tailOnce()
		e.maintainOnce()
	}

	if got := kicks.Load(); got != 1 {
		t.Fatalf("kicks = %d, want 1 on %s without fail2ban", got, "any OS")
	}
}

// --- 12. no per-event database writes ----------------------------------------

func TestClientLimitNoPerEventDBWrites(t *testing.T) {
	clock := newFakeClock()
	e, path := newLimitEngineForTest(t, clock, nil)
	createLimitInbound(t, []model.Client{{ID: "uuid-1", Email: "user@test", LimitIP: 1, Enable: true}})

	// Pre-existing IP history row: it must not be touched by the engine.
	db := database.GetDB()
	if err := db.Create(&model.InboundClientIps{ClientEmail: "user@test", Ips: "[]"}).Error; err != nil {
		t.Fatalf("seed inbound_client_ips: %v", err)
	}
	trafficBefore := reloadTrafficRow(t, "user@test")

	// A burst of events from two IPs.
	for i := 0; i < 50; i++ {
		ip := "1.1.1.1"
		if i%2 == 1 {
			ip = "2.2.2.2"
		}
		appendAccessLog(t, path, limitLine(clock.Now(), ip, "user@test"))
		e.tailOnce()
		e.maintainOnce()
	}

	// The stored history row is unchanged and client_traffics untouched.
	var ipsRow model.InboundClientIps
	if err := db.Where("client_email = ?", "user@test").First(&ipsRow).Error; err != nil {
		t.Fatalf("load inbound_client_ips: %v", err)
	}
	if ipsRow.Ips != "[]" {
		t.Fatalf("inbound_client_ips.ips = %q, want unchanged %q", ipsRow.Ips, "[]")
	}
	trafficAfter := reloadTrafficRow(t, "user@test")
	if trafficAfter.Up != trafficBefore.Up || trafficAfter.Down != trafficBefore.Down || trafficAfter.Enable != trafficBefore.Enable {
		t.Fatalf("client_traffics changed by engine: %+v -> %+v", trafficBefore, trafficAfter)
	}

	// The live view still sees both IPs (served from memory).
	live := clientLimitEngineInstLiveIPsForTest(e, "user@test")
	if len(live) != 2 {
		t.Fatalf("live IPs = %v, want 2", live)
	}
}

func clientLimitEngineInstLiveIPsForTest(e *clientLimitEngine, email string) []ClientIPWithTimestamp {
	saved := clientLimitEngineInst
	clientLimitEngineInst = e
	defer func() { clientLimitEngineInst = saved }()
	return clientLimitEngineLiveIPs(email)
}

// --- 13. 100k-client cardinality ---------------------------------------------

func TestClientLimit100kClientsScalability(t *testing.T) {
	clock := newFakeClock()
	e, path := newLimitEngineForTest(t, clock, func(email string, p *clientIPPolicy) error { return nil })

	// Seed the policy cache and live state for 100k clients directly (one
	// active IP each, no violations) so maintainOnce has real work to do
	// without 100k database rows.
	const n = 100_000
	now := clock.Now().Unix()
	e.mu.Lock()
	for i := 0; i < n; i++ {
		email := fmt.Sprintf("client%06d@test", i)
		e.policy[email] = &clientIPPolicy{
			inboundId: 1, tag: "in", protocol: "vmess",
			limitIP: 1, graceSec: defaultClientIPGrace, enable: true,
			loadedAt: now,
		}
		e.clients[email] = &clientIPState{ips: map[string]*clientIPSighting{
			fmt.Sprintf("10.%d.%d.%d", (i>>16)%256, (i>>8)%256, i%256): {firstSeen: now, lastSeen: now},
		}}
	}
	e.mu.Unlock()

	start := time.Now()
	e.maintainOnce()
	elapsed := time.Now().Sub(start)
	if elapsed > 30*time.Second {
		t.Fatalf("maintainOnce over 100k clients took %v, too slow", elapsed)
	}

	// A burst of 10k log lines for a single heavy client must also stay
	// proportional to the event count.
	e.policy["heavy@test"] = &clientIPPolicy{
		inboundId: 1, tag: "in", protocol: "vmess",
		limitIP: 1000, graceSec: defaultClientIPGrace, enable: true,
		loadedAt: now,
	}
	var burst string
	for i := 0; i < 10_000; i++ {
		burst += limitLine(clock.Now(), "192.168.1.1", "heavy@test")
	}
	appendAccessLog(t, path, burst)
	start = time.Now()
	e.tailOnce()
	elapsed = time.Now().Sub(start)
	if elapsed > 30*time.Second {
		t.Fatalf("tailOnce over 10k events took %v, too slow", elapsed)
	}
	e.mu.Lock()
	events := e.eventsObserved
	e.mu.Unlock()
	if events != 10_000 {
		t.Fatalf("eventsObserved = %d, want 10000", events)
	}
}

// --- Hardening pass: per-client switch semantics (Tests A-I) -----------------

// setInboundClients rewrites an inbound's client list in the settings blob,
// mirroring what UpdateInboundClient persists.
func setInboundClients(t *testing.T, inboundId int, clients []model.Client) {
	t.Helper()
	db := database.GetDB()
	settings := map[string]any{"clients": clients}
	body, err := json.Marshal(settings)
	if err != nil {
		t.Fatalf("marshal settings: %v", err)
	}
	if err := db.Model(model.Inbound{}).Where("id = ?", inboundId).Update("settings", string(body)).Error; err != nil {
		t.Fatalf("update settings: %v", err)
	}
}

// invalidatePolicy is the test-side stand-in for the UpdateInboundClient
// save hook (clientLimitEngineNotifyClientUpdated), applied to the local
// engine under test rather than the package singleton.
func invalidatePolicy(e *clientLimitEngine, email string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.policy, email)
	delete(e.negative, email)
}

// TestA: OFF client and ON client in the same inbound are independent.
func TestPerClientSwitchA_OffAndOnIndependence(t *testing.T) {
	clock := newFakeClock()
	var kicksA, kicksB atomic.Int32
	e, path := newLimitEngineForTest(t, clock, func(email string, p *clientIPPolicy) error {
		if email == "a@test" {
			kicksA.Add(1)
		} else {
			kicksB.Add(1)
		}
		return nil
	})
	createLimitInbound(t, []model.Client{
		{ID: "u1", Email: "a@test", LimitIP: 0, Enable: true}, // OFF
		{ID: "u2", Email: "b@test", LimitIP: 1, Enable: true}, // ON, Max=1
	})

	// Both clients show two distinct IPs; only B may ever be enforced.
	for i := 0; i < 5; i++ {
		appendAccessLog(t, path,
			limitLine(clock.Now(), "1.1.1.1", "a@test")+
				limitLine(clock.Now(), "2.2.2.2", "a@test")+
				limitLine(clock.Now(), "1.1.1.1", "b@test")+
				limitLine(clock.Now(), "2.2.2.2", "b@test"))
		e.tailOnce()
		e.maintainOnce()
		clock.Advance(30 * time.Second)
	}
	if got := kicksA.Load(); got != 0 {
		t.Fatalf("OFF client A kicked %d times, want 0", got)
	}
	if got := kicksB.Load(); got != 1 {
		t.Fatalf("ON client B kicked %d times, want 1", got)
	}
}

// TestB: three clients, mixed ON/OFF — policies are fully independent.
func TestPerClientSwitchB_ThreeClientsIndependent(t *testing.T) {
	clock := newFakeClock()
	var kicksB, kicksC atomic.Int32
	e, path := newLimitEngineForTest(t, clock, func(email string, p *clientIPPolicy) error {
		switch email {
		case "b@test":
			kicksB.Add(1)
		case "c@test":
			kicksC.Add(1)
		}
		return nil
	})
	createLimitInbound(t, []model.Client{
		{ID: "u1", Email: "a@test", LimitIP: 0, Enable: true},
		{ID: "u2", Email: "b@test", LimitIP: 2, Enable: true},
		{ID: "u3", Email: "c@test", LimitIP: 1, Enable: true},
	})

	// A and B stay within their policies (A off, B limit 2 with 2 IPs);
	// C exceeds (3 IPs against limit 1) and is enforced.
	for i := 0; i < 5; i++ {
		appendAccessLog(t, path,
			limitLine(clock.Now(), "1.1.1.1", "a@test")+
				limitLine(clock.Now(), "9.9.9.9", "a@test")+
				limitLine(clock.Now(), "1.1.1.1", "b@test")+
				limitLine(clock.Now(), "2.2.2.2", "b@test")+
				limitLine(clock.Now(), "1.1.1.1", "c@test")+
				limitLine(clock.Now(), "2.2.2.2", "c@test")+
				limitLine(clock.Now(), "3.3.3.3", "c@test"))
		e.tailOnce()
		e.maintainOnce()
		clock.Advance(30 * time.Second)
	}
	if got := kicksB.Load(); got != 0 {
		t.Fatalf("client B (limit 2, 2 IPs) kicked %d times, want 0", got)
	}
	if got := kicksC.Load(); got != 1 {
		t.Fatalf("client C (limit 1, 3 IPs) kicked %d times, want 1", got)
	}
}

// TestC: ON -> OFF cancels pending enforcement and cleans state.
func TestPerClientSwitchC_OnToOffCancelsPending(t *testing.T) {
	clock := newFakeClock()
	var kicks atomic.Int32
	e, path := newLimitEngineForTest(t, clock, func(email string, p *clientIPPolicy) error {
		kicks.Add(1)
		return nil
	})
	inbound := createLimitInbound(t, []model.Client{{ID: "u1", Email: "a@test", LimitIP: 1, Enable: true}})

	// Violation with both IPs staying active: the pending window starts and
	// ripens to enforcement at t+60s, proving the setup kicks on schedule
	// (maintain passes at t=0/25/50/75 — the last one is past the deadline).
	for i := 0; i < 4; i++ {
		appendAccessLog(t, path, limitLine(clock.Now(), "1.1.1.1", "a@test")+limitLine(clock.Now(), "2.2.2.2", "a@test"))
		e.tailOnce()
		e.maintainOnce()
		clock.Advance(25 * time.Second)
	}
	if got := kicks.Load(); got != 1 {
		t.Fatalf("control: kicks = %d, want 1 (violation must ripen first)", got)
	}

	// Now a SECOND violation is pending (post-kick state has one IP; a new
	// second IP re-opens a pending window)...
	e.mu.Lock()
	if st := e.clients["a@test"]; st != nil {
		st.pendingSince = 0 // kick cooldown resets cleanly
	}
	e.mu.Unlock()
	appendAccessLog(t, path, limitLine(clock.Now(), "2.2.2.2", "a@test")+limitLine(clock.Now(), "3.3.3.3", "a@test"))
	e.tailOnce()
	e.maintainOnce()
	e.mu.Lock()
	pendingSet := e.clients["a@test"] != nil && e.clients["a@test"].pendingSince != 0
	e.mu.Unlock()
	if !pendingSet {
		t.Fatalf("violation not pending before the switch-off")
	}

	// Admin turns the switch OFF (limitIp -> 0) and the client is saved;
	// UpdateInboundClient's hook invalidates the cached policy.
	setInboundClients(t, inbound.Id, []model.Client{{ID: "u1", Email: "a@test", LimitIP: 0, Enable: true}})
	invalidatePolicy(e, "a@test")

	// Ride out the grace window with continued traffic: no further kick, no
	// state left.
	for i := 0; i < 5; i++ {
		clock.Advance(30 * time.Second)
		appendAccessLog(t, path, limitLine(clock.Now(), "1.1.1.1", "a@test")+limitLine(clock.Now(), "2.2.2.2", "a@test"))
		e.tailOnce()
		e.maintainOnce()
	}
	if got := kicks.Load(); got != 1 {
		t.Fatalf("kicks after switch-off = %d, want still 1 (no new enforcement)", got)
	}
	e.mu.Lock()
	stillTracked := e.clients["a@test"] != nil
	e.mu.Unlock()
	if stillTracked {
		t.Fatalf("OFF client still has engine state; must be cleaned")
	}
}

// TestD: OFF -> ON activates the policy without restart.
func TestPerClientSwitchD_OffToOnActivates(t *testing.T) {
	clock := newFakeClock()
	var kicks atomic.Int32
	e, path := newLimitEngineForTest(t, clock, func(email string, p *clientIPPolicy) error {
		kicks.Add(1)
		return nil
	})
	inbound := createLimitInbound(t, []model.Client{{ID: "u1", Email: "a@test", LimitIP: 0, Enable: true}})

	// Traffic while OFF: nothing tracked.
	for i := 0; i < 3; i++ {
		appendAccessLog(t, path, limitLine(clock.Now(), "1.1.1.1", "a@test")+limitLine(clock.Now(), "2.2.2.2", "a@test"))
		e.tailOnce()
		e.maintainOnce()
		clock.Advance(20 * time.Second)
	}
	e.mu.Lock()
	tracked := len(e.clients)
	e.mu.Unlock()
	if tracked != 0 {
		t.Fatalf("tracked clients while OFF = %d, want 0", tracked)
	}

	// Admin enables the switch (limitIp 0 -> 1) and saves.
	setInboundClients(t, inbound.Id, []model.Client{{ID: "u1", Email: "a@test", LimitIP: 1, Enable: true}})
	invalidatePolicy(e, "a@test")

	// The policy is now active: the two-IP traffic leads to a kick after
	// the grace window, with no restart of anything.
	for i := 0; i < 5; i++ {
		appendAccessLog(t, path, limitLine(clock.Now(), "1.1.1.1", "a@test")+limitLine(clock.Now(), "2.2.2.2", "a@test"))
		e.tailOnce()
		e.maintainOnce()
		clock.Advance(30 * time.Second)
	}
	if got := kicks.Load(); got != 1 {
		t.Fatalf("kicks after switch-on = %d, want 1", got)
	}
}

// TestE: raising Max IP while two IPs are active dissolves the violation
// without a kick.
func TestPerClientSwitchE_MaxIpRaiseNoKick(t *testing.T) {
	clock := newFakeClock()
	var kicks atomic.Int32
	e, path := newLimitEngineForTest(t, clock, func(email string, p *clientIPPolicy) error {
		kicks.Add(1)
		return nil
	})
	inbound := createLimitInbound(t, []model.Client{{ID: "u1", Email: "a@test", LimitIP: 1, Enable: true}})

	// Violation pending: 2 IPs against limit 1, inside the grace window.
	appendAccessLog(t, path, limitLine(clock.Now(), "1.1.1.1", "a@test")+limitLine(clock.Now(), "2.2.2.2", "a@test"))
	e.tailOnce()
	e.maintainOnce()
	e.mu.Lock()
	pendingSet := e.clients["a@test"].pendingSince != 0
	e.mu.Unlock()
	if !pendingSet {
		t.Fatalf("violation not pending before the Max IP raise")
	}

	// Admin raises Max IP 1 -> 2 and saves. The pending violation must
	// dissolve — configuration changes never kick on their own.
	setInboundClients(t, inbound.Id, []model.Client{{ID: "u1", Email: "a@test", LimitIP: 2, Enable: true}})
	invalidatePolicy(e, "a@test")

	// Ride out what WOULD have been the grace deadline with both IPs active.
	for i := 0; i < 5; i++ {
		clock.Advance(30 * time.Second)
		appendAccessLog(t, path, limitLine(clock.Now(), "1.1.1.1", "a@test")+limitLine(clock.Now(), "2.2.2.2", "a@test"))
		e.tailOnce()
		e.maintainOnce()
	}
	if got := kicks.Load(); got != 0 {
		t.Fatalf("kicks after Max IP raise = %d, want 0 (no kick purely from config change)", got)
	}
	if ips := trackedIPs(e, "a@test"); len(ips) != 2 {
		t.Fatalf("tracked IPs = %d, want 2", len(ips))
	}
}

// TestF: changing the grace period mid-violation is respected by the new
// policy.
func TestPerClientSwitchF_GraceChangeRespected(t *testing.T) {
	clock := newFakeClock()
	var kicks atomic.Int32
	e, path := newLimitEngineForTest(t, clock, func(email string, p *clientIPPolicy) error {
		kicks.Add(1)
		return nil
	})
	inbound := createLimitInbound(t, []model.Client{
		{ID: "u1", Email: "a@test", LimitIP: 1, LimitGrace: 60, Enable: true},
	})

	// Violation pending under a 60s grace (single pass: starts the window
	// without ripening it, 0s elapsed so far).
	appendAccessLog(t, path, limitLine(clock.Now(), "1.1.1.1", "a@test")+limitLine(clock.Now(), "2.2.2.2", "a@test"))
	e.tailOnce()
	e.maintainOnce()
	e.mu.Lock()
	pendingSet := e.clients["a@test"].pendingSince != 0
	e.mu.Unlock()
	if !pendingSet {
		t.Fatalf("violation not pending before the grace change")
	}

	// Admin extends the grace 60 -> 120 and saves.
	setInboundClients(t, inbound.Id, []model.Client{{ID: "u1", Email: "a@test", LimitIP: 1, LimitGrace: 120, Enable: true}})
	invalidatePolicy(e, "a@test")

	// At ~66s (past the OLD grace) the new policy still holds: no kick.
	for i := 0; i < 2; i++ {
		clock.Advance(33 * time.Second)
		appendAccessLog(t, path, limitLine(clock.Now(), "1.1.1.1", "a@test")+limitLine(clock.Now(), "2.2.2.2", "a@test"))
		e.tailOnce()
		e.maintainOnce()
	}
	if got := kicks.Load(); got != 0 {
		t.Fatalf("kicked before the new 120s grace elapsed")
	}

	// Past 120s of continuous violation, the new policy enforces.
	for i := 0; i < 3; i++ {
		clock.Advance(33 * time.Second)
		appendAccessLog(t, path, limitLine(clock.Now(), "1.1.1.1", "a@test")+limitLine(clock.Now(), "2.2.2.2", "a@test"))
		e.tailOnce()
		e.maintainOnce()
	}
	if got := kicks.Load(); got != 1 {
		t.Fatalf("kicks = %d after new grace elapsed, want 1", got)
	}
}

// TestG: deleting a client with active engine state is clean.
func TestPerClientSwitchG_DeleteActiveClient(t *testing.T) {
	clock := newFakeClock()
	var kicks atomic.Int32
	e, path := newLimitEngineForTest(t, clock, func(email string, p *clientIPPolicy) error {
		kicks.Add(1)
		return nil
	})
	inbound := createLimitInbound(t, []model.Client{{ID: "u1", Email: "a@test", LimitIP: 1, Enable: true}})

	// Active state + a pending violation (single pass: tracks 2 IPs, starts
	// the pending window, does not ripen it).
	appendAccessLog(t, path, limitLine(clock.Now(), "1.1.1.1", "a@test")+limitLine(clock.Now(), "2.2.2.2", "a@test"))
	e.tailOnce()
	e.maintainOnce()
	e.mu.Lock()
	pendingSet := e.clients["a@test"].pendingSince != 0
	e.mu.Unlock()
	if !pendingSet {
		t.Fatalf("violation not pending before the delete")
	}

	// Client deleted: settings row and stats row removed, hook fires (the
	// DelClientIPs path calls clientLimitEngineForgetClient).
	setInboundClients(t, inbound.Id, []model.Client{})
	database.GetDB().Where("email = ?", "a@test").Delete(xray.ClientTraffic{})
	// Client deleted: the DelClientIPs path calls the forget hook on the
	// live engine; this mirrors it for the local engine under test.
	e.mu.Lock()
	delete(e.clients, "a@test")
	delete(e.policy, "a@test")
	delete(e.negative, "a@test")
	e.mu.Unlock()

	// Continued traffic for the deleted email: no panic, no state, no kick.
	for i := 0; i < 5; i++ {
		clock.Advance(30 * time.Second)
		appendAccessLog(t, path, limitLine(clock.Now(), "1.1.1.1", "a@test")+limitLine(clock.Now(), "2.2.2.2", "a@test"))
		e.tailOnce()
		e.maintainOnce()
	}
	e.mu.Lock()
	stillTracked := e.clients["a@test"] != nil
	e.mu.Unlock()
	if stillTracked {
		t.Fatalf("deleted client still tracked")
	}
	if got := kicks.Load(); got != 0 {
		t.Fatalf("kicks for deleted client = %d, want 0", got)
	}
}

// TestH: reseller-owned client integrity after enforcement (owner, enable
// flag, disable reason all untouched).
func TestPerClientSwitchH_ResellerOwnershipIntact(t *testing.T) {
	clock := newFakeClock()
	var kicks atomic.Int32
	e, path := newLimitEngineForTest(t, clock, func(email string, p *clientIPPolicy) error {
		kicks.Add(1)
		return nil
	})
	ownerID := 42
	inbound := createLimitInbound(t, []model.Client{
		{ID: "u1", Email: "resold@test", LimitIP: 1, Enable: true, OwnerUserID: ownerID},
	})

	// Drive enforcement.
	appendAccessLog(t, path, limitLine(clock.Now(), "1.1.1.1", "resold@test")+limitLine(clock.Now(), "2.2.2.2", "resold@test"))
	e.tailOnce()
	e.maintainOnce()
	for i := 0; i < 4; i++ {
		clock.Advance(25 * time.Second)
		appendAccessLog(t, path, limitLine(clock.Now(), "1.1.1.1", "resold@test")+limitLine(clock.Now(), "2.2.2.2", "resold@test"))
		e.tailOnce()
		e.maintainOnce()
	}
	if got := kicks.Load(); got != 1 {
		t.Fatalf("kicks = %d, want 1", got)
	}

	clients, err := (&InboundService{}).GetClients(inbound)
	if err != nil {
		t.Fatalf("GetClients: %v", err)
	}
	if len(clients) != 1 {
		t.Fatalf("clients = %d, want 1", len(clients))
	}
	c := clients[0]
	if c.OwnerUserID != ownerID {
		t.Fatalf("OwnerUserID = %d, want %d (ownership must survive enforcement)", c.OwnerUserID, ownerID)
	}
	if !c.Enable {
		t.Fatalf("client Enable was flipped by enforcement")
	}
	if c.DisableReason != "" {
		t.Fatalf("DisableReason = %q, want empty (enforcement is not a disable)", c.DisableReason)
	}
}

// TestI: 100k configured clients with only 100 IP-limit clients must not
// cause a 100k-client periodic scan. The engine only walks clients that have
// live state; configured-but-untracked clients never enter the state map.
func TestPerClientSwitchI_100kClientsOnly100Tracked(t *testing.T) {
	clock := newFakeClock()
	e, path := newLimitEngineForTest(t, clock, func(email string, p *clientIPPolicy) error { return nil })

	const total = 100_000
	const limited = 100
	now := clock.Now().Unix()
	e.mu.Lock()
	// 100k clients are "configured" (policy cache entries model resolved
	// policies) but only 100 have IP limiting on, and only those carry live
	// state.
	for i := 0; i < total; i++ {
		email := fmt.Sprintf("client%06d@test", i)
		e.policy[email] = &clientIPPolicy{
			inboundId: 1, tag: "in", protocol: "vmess",
			limitIP: 0, graceSec: defaultClientIPGrace, // limit OFF
			enable: true, loadedAt: now,
		}
		if i < limited {
			e.policy[email].limitIP = 1
			e.clients[email] = &clientIPState{ips: map[string]*clientIPSighting{
				"10.0.0.1": {firstSeen: now, lastSeen: now},
			}}
		}
	}
	e.mu.Unlock()

	// The maintain pass must walk only the 100 tracked clients, not 100k.
	e.mu.Lock()
	trackedCount := len(e.clients)
	policyCount := len(e.policy)
	e.mu.Unlock()
	if trackedCount != limited {
		t.Fatalf("tracked clients = %d, want %d", trackedCount, limited)
	}
	if policyCount != total {
		t.Fatalf("policy cache = %d, want %d (configured clients)", policyCount, total)
	}

	start := time.Now()
	e.maintainOnce()
	elapsed := time.Now().Sub(start)
	if elapsed > 10*time.Second {
		t.Fatalf("maintainOnce with %d configured / %d tracked took %v, too slow", total, limited, elapsed)
	}

	// Events for a non-limited client cost one cache hit and no state.
	for i := 0; i < 1000; i++ {
		appendAccessLog(t, path, limitLine(clock.Now(), "172.16.0.1", "client99999@test"))
	}
	e.tailOnce()
	e.mu.Lock()
	trackedAfter := len(e.clients)
	e.mu.Unlock()
	if trackedAfter != limited {
		t.Fatalf("tracked clients after OFF-client events = %d, want still %d", trackedAfter, limited)
	}
}

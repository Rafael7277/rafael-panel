package service

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mhsanaei/3x-ui/v2/database/model"
)

// fakeRuleAPI records every AddRoutingRule/RemoveRoutingRule call so tests
// can assert on the exact call sequence. It implements xrayRuleAPI.
type fakeRuleAPI struct {
	mu      sync.Mutex
	adds    []string // "tag email ip" per AddRoutingRule
	removes []string // tag per RemoveRoutingRule
	addErr  error    // when set, AddRoutingRule fails (fallback tests)
}

func (f *fakeRuleAPI) AddRoutingRule(ruleTag, userEmail, ip string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addErr != nil {
		return f.addErr
	}
	f.adds = append(f.adds, fmt.Sprintf("%s %s %s", ruleTag, userEmail, ip))
	return nil
}

func (f *fakeRuleAPI) RemoveRoutingRule(ruleTag string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removes = append(f.removes, ruleTag)
	return nil
}

// newRouterForTest wires a ClientLimitRouter to a fresh fake API.
func newRouterForTest(t *testing.T) (*ClientLimitRouter, *fakeRuleAPI) {
	t.Helper()
	api := &fakeRuleAPI{}
	r := newClientLimitRouter()
	r.dialFn = func() (xrayRuleAPI, error) { return api, nil }
	return r, api
}

// --- T1: Enforce installs a rule per violating IP --------------------------

func TestLimitRouterEnforceBuildsRule(t *testing.T) {
	r, api := newRouterForTest(t)
	r.Enforce("user@test", []string{"1.2.3.4"})

	if len(api.adds) != 1 {
		t.Fatalf("adds = %d, want 1", len(api.adds))
	}
	add := api.adds[0]
	wantTag := ruleTagFor("user@test", "1.2.3.4")
	if add != wantTag+" user@test 1.2.3.4" {
		t.Fatalf("add = %q, want %q", add, wantTag+" user@test 1.2.3.4")
	}
	// Pre-remove before add: exactly one RemoveRule precedes the AddRule.
	if len(api.removes) != 1 || api.removes[0] != wantTag {
		t.Fatalf("removes = %v, want [%s]", api.removes, wantTag)
	}
	snap := r.Snapshot()
	if len(snap) != 1 || snap[0].Email != "user@test" || snap[0].IP != "1.2.3.4" || snap[0].Pending {
		t.Fatalf("snapshot = %+v, want one non-pending rule for user@test/1.2.3.4", snap)
	}
}

// --- T2: Release removes the rule with the same tag -------------------------

func TestLimitRouterReleaseRemovesRule(t *testing.T) {
	r, api := newRouterForTest(t)
	r.Enforce("user@test", []string{"1.2.3.4"})
	r.Release("user@test", "1.2.3.4")

	wantTag := ruleTagFor("user@test", "1.2.3.4")
	// One pre-remove (Enforce) + one release remove.
	if len(api.removes) != 2 || api.removes[1] != wantTag {
		t.Fatalf("removes = %v, want second remove = %s", api.removes, wantTag)
	}
	if got := len(r.Snapshot()); got != 0 {
		t.Fatalf("snapshot size = %d, want 0 after release", got)
	}
}

// --- T3: Enforce is idempotent ----------------------------------------------

func TestLimitRouterEnforceIdempotent(t *testing.T) {
	r, api := newRouterForTest(t)
	r.Enforce("user@test", []string{"5.5.5.5"})
	r.Enforce("user@test", []string{"5.5.5.5"})

	// Second Enforce on an installed rule must be a no-op (no extra RPCs).
	if len(api.adds) != 1 {
		t.Fatalf("adds = %d, want 1 (idempotent)", len(api.adds))
	}
	if len(api.removes) != 1 {
		t.Fatalf("removes = %d, want 1 (idempotent)", len(api.removes))
	}
}

// --- T4: gRPC failure is non-fatal and retried ------------------------------

func TestLimitRouterFallbackOnApiError(t *testing.T) {
	api := &fakeRuleAPI{}
	r := newClientLimitRouter()
	r.dialFn = func() (xrayRuleAPI, error) { return api, nil }

	// First attempt fails at the gRPC layer.
	api.addErr = errors.New("RoutingService not available")
	r.Enforce("user@test", []string{"1.2.3.4"})
	snap := r.Snapshot()
	if len(snap) != 1 || !snap[0].Pending {
		t.Fatalf("snapshot = %+v, want one PENDING rule (failed add, tracked for retry)", snap)
	}

	// Recovery: the next Enforce pass retries and succeeds.
	api.addErr = nil
	r.Enforce("user@test", []string{"1.2.3.4"})
	snap = r.Snapshot()
	if len(snap) != 1 || snap[0].Pending {
		t.Fatalf("snapshot = %+v, want one non-pending rule after retry", snap)
	}
	if len(api.adds) != 1 {
		t.Fatalf("adds = %d, want 1 successful add after retry", len(api.adds))
	}

	// Dial failure must also be non-fatal (no panic, no rule installed).
	r2 := newClientLimitRouter()
	r2.dialFn = func() (xrayRuleAPI, error) { return nil, errors.New("unreachable") }
	r2.Enforce("user@test", []string{"9.9.9.9"})
	if got := len(r2.Snapshot()); got != 0 {
		t.Fatalf("snapshot = %d, want 0 when API is unreachable", got)
	}
}

// --- T5: engine installs the rule on violation ------------------------------

func TestEngineInstallsRuleOnViolation(t *testing.T) {
	clock := newFakeClock()
	var kicks int
	e, path := newLimitEngineForTest(t, clock, func(email string, p *clientIPPolicy) error {
		kicks++
		return nil
	})
	r, api := newRouterForTest(t)
	e.limitRouter = r
	createLimitInbound(t, []model.Client{{ID: "uuid-1", Email: "user@test", LimitIP: 1, Enable: true}})

	// IP A connects, then IP B joins and both stay active past the grace.
	appendAccessLog(t, path, limitLine(clock.Now(), "1.1.1.1", "user@test"))
	e.tailOnce()
	e.maintainOnce()
	appendAccessLog(t, path, limitLine(clock.Now(), "2.2.2.2", "user@test"))
	e.tailOnce()
	e.maintainOnce()

	for i := 0; i < 3; i++ {
		clock.Advance(25 * time.Second)
		appendAccessLog(t, path, limitLine(clock.Now(), "1.1.1.1", "user@test")+limitLine(clock.Now(), "2.2.2.2", "user@test"))
		e.tailOnce()
		e.maintainOnce()
	}

	if kicks == 0 {
		t.Fatal("expected at least one kick (sanity)")
	}
	if len(api.adds) != 1 {
		t.Fatalf("routing rules installed = %d, want 1 (the older IP 1.1.1.1)", len(api.adds))
	}
	add := api.adds[0]
	// The OLDER IP is the violator (newest-kept policy: 2.2.2.2 survives).
	if !containsStrField(add, "1.1.1.1") {
		t.Fatalf("rule add = %q, want it to block 1.1.1.1", add)
	}
	if containsStrField(add, "2.2.2.2") {
		t.Fatalf("rule add = %q, must not block the kept IP 2.2.2.2", add)
	}
}

// --- T6: rule is released when the client drops back under the limit --------

func TestEngineReleasesRuleWhenUnderLimit(t *testing.T) {
	clock := newFakeClock()
	e, path := newLimitEngineForTest(t, clock, func(email string, p *clientIPPolicy) error { return nil })
	r, api := newRouterForTest(t)
	e.limitRouter = r
	createLimitInbound(t, []model.Client{{ID: "uuid-1", Email: "user@test", LimitIP: 1, Enable: true}})

	// Violation: two IPs.
	appendAccessLog(t, path, limitLine(clock.Now(), "1.1.1.1", "user@test"))
	e.tailOnce()
	e.maintainOnce()
	appendAccessLog(t, path, limitLine(clock.Now(), "2.2.2.2", "user@test"))
	e.tailOnce()
	e.maintainOnce()
	for i := 0; i < 3; i++ {
		clock.Advance(25 * time.Second)
		appendAccessLog(t, path, limitLine(clock.Now(), "1.1.1.1", "user@test")+limitLine(clock.Now(), "2.2.2.2", "user@test"))
		e.tailOnce()
		e.maintainOnce()
	}
	if len(api.adds) != 1 {
		t.Fatalf("rules installed = %d, want 1 before recovery", len(api.adds))
	}

	// The kept IP (2.2.2.2) keeps working; 1.1.1.1 never returns. After its
	// TTL (grace default 60s) expires, the rule must be released.
	for i := 0; i < 6; i++ {
		clock.Advance(25 * time.Second)
		appendAccessLog(t, path, limitLine(clock.Now(), "2.2.2.2", "user@test"))
		e.tailOnce()
		e.maintainOnce()
	}
	if got := len(r.Snapshot()); got != 0 {
		t.Fatalf("active rules = %d, want 0 after TTL expiry of the violating IP", got)
	}
	wantTag := ruleTagFor("user@test", "1.1.1.1")
	found := false
	for _, tag := range api.removes {
		if tag == wantTag {
			found = true
		}
	}
	if !found {
		t.Fatalf("RemoveRoutingRule for %s never issued; removes = %v", wantTag, api.removes)
	}
}

// --- T7: disabling the client lifts all its rules ---------------------------

func TestEngineReleaseAllOnClientDisable(t *testing.T) {
	clock := newFakeClock()
	e, path := newLimitEngineForTest(t, clock, func(email string, p *clientIPPolicy) error { return nil })
	r, api := newRouterForTest(t)
	e.limitRouter = r
	createLimitInbound(t, []model.Client{{ID: "uuid-1", Email: "user@test", LimitIP: 1, Enable: true}})

	// Violation and rule installation.
	appendAccessLog(t, path, limitLine(clock.Now(), "1.1.1.1", "user@test"))
	e.tailOnce()
	e.maintainOnce()
	appendAccessLog(t, path, limitLine(clock.Now(), "2.2.2.2", "user@test"))
	e.tailOnce()
	e.maintainOnce()
	for i := 0; i < 3; i++ {
		clock.Advance(25 * time.Second)
		appendAccessLog(t, path, limitLine(clock.Now(), "1.1.1.1", "user@test")+limitLine(clock.Now(), "2.2.2.2", "user@test"))
		e.tailOnce()
		e.maintainOnce()
	}
	if len(api.adds) != 1 {
		t.Fatalf("rules installed = %d, want 1 before disable", len(api.adds))
	}

	// The notify hook operates on the package-level engine; make that the
	// test engine for the call (same save/restore pattern as the live-IPs
	// helper in client_limit_engine_test.go).
	saved := clientLimitEngineInst
	clientLimitEngineInst = e
	clientLimitEngineNotifyClientUpdated("user@test", "")
	clientLimitEngineInst = saved
	if got := len(r.Snapshot()); got != 0 {
		t.Fatalf("active rules = %d, want 0 after client disable", got)
	}
}

// --- T8: rule survives inside cooldown (not released early) -----------------

func TestEngineKeepsRuleDuringCooldownBetweenKicks(t *testing.T) {
	clock := newFakeClock()
	e, path := newLimitEngineForTest(t, clock, func(email string, p *clientIPPolicy) error { return nil })
	r, api := newRouterForTest(t)
	e.limitRouter = r
	createLimitInbound(t, []model.Client{{ID: "uuid-1", Email: "user@test", LimitIP: 1, Enable: true}})

	// Violation with both IPs continuously active.
	appendAccessLog(t, path, limitLine(clock.Now(), "1.1.1.1", "user@test"))
	e.tailOnce()
	e.maintainOnce()
	appendAccessLog(t, path, limitLine(clock.Now(), "2.2.2.2", "user@test"))
	e.tailOnce()
	e.maintainOnce()
	for i := 0; i < 3; i++ {
		clock.Advance(25 * time.Second)
		appendAccessLog(t, path, limitLine(clock.Now(), "1.1.1.1", "user@test")+limitLine(clock.Now(), "2.2.2.2", "user@test"))
		e.tailOnce()
		e.maintainOnce()
	}
	if len(api.adds) != 1 {
		t.Fatalf("rules installed = %d, want 1", len(api.adds))
	}

	// Both IPs STILL active: no release may happen even while the kick is
	// in cooldown — the violating IP must stay blackholed.
	for i := 0; i < 4; i++ {
		clock.Advance(25 * time.Second)
		appendAccessLog(t, path, limitLine(clock.Now(), "1.1.1.1", "user@test")+limitLine(clock.Now(), "2.2.2.2", "user@test"))
		e.tailOnce()
		e.maintainOnce()
	}
	if got := len(r.Snapshot()); got != 1 {
		t.Fatalf("active rules = %d, want 1 (rule must persist while violation persists)", got)
	}
	// And no repeated re-install churn: still exactly one AddRule total.
	if len(api.adds) != 1 {
		t.Fatalf("adds = %d, want 1 (no churn while rule is active)", len(api.adds))
	}
}

// containsStrField: simple substring helper for the "tag email ip" record.
func containsStrField(record, s string) bool {
	return strings.Contains(" "+record+" ", " "+s+" ")
}

// tracker_test.go covers iplimit.md §19 tests 1-6.
package iplimit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newTestTracker(limits map[string]limitEntry) *Tracker {
	t := &Tracker{
		limits: limits,
		active: make(map[string]map[string]*slot),
	}
	if t.limits == nil {
		t.limits = make(map[string]limitEntry)
	}
	return t
}

// T1: within limit — first IP allowed.
func TestTrackerAllowsWithinLimit(t *testing.T) {
	tr := newTestTracker(map[string]limitEntry{"user@test": {LimitIP: 1}})
	now := time.Now()

	ok, rel := tr.Check("user@test", "1.1.1.1", now)
	if !ok {
		t.Fatal("first IP must be allowed")
	}
	if rel == nil {
		t.Fatal("allowed connection must return a release func")
	}
	rel()
}

// T2: second distinct IP rejected while the first is connected.
func TestTrackerRejectsSecondIP(t *testing.T) {
	tr := newTestTracker(map[string]limitEntry{"user@test": {LimitIP: 1}})
	now := time.Now()

	ok1, rel1 := tr.Check("user@test", "1.1.1.1", now)
	if !ok1 {
		t.Fatal("first IP must be allowed")
	}
	ok2, _ := tr.Check("user@test", "2.2.2.2", now)
	if ok2 {
		t.Fatal("second distinct IP must be REJECTED while first is live")
	}
	rel1()

	// After release + TTL expiry the slot frees up.
	later := now.Add(2 * defaultTTL)
	ok3, rel3 := tr.Check("user@test", "2.2.2.2", later)
	if !ok3 {
		t.Fatal("after TTL expiry the new IP must be allowed")
	}
	rel3()
}

// T3: release on connection close frees the slot for another IP.
func TestTrackerReleasesOnConnectionClose(t *testing.T) {
	tr := newTestTracker(map[string]limitEntry{"user@test": {LimitIP: 1}})
	now := time.Now()

	_, rel := tr.Check("user@test", "1.1.1.1", now)
	rel()

	// Same instant, different IP with zero live connections: the gate is
	// connection-synchronous, so the freed quota is available AT ONCE.
	// (Stricter than phase 1: there the grace TTL kept the old IP counted
	// for reconnect protection; here the slot is warm only for the SAME
	// ip — a different ip takes the free slot immediately. This is the
	// zero-delay semantics the phase-2 gate exists for.)
	ok, rel2 := tr.Check("user@test", "2.2.2.2", now)
	if !ok {
		t.Fatal("freed slot (no live conns) must be immediately available to a new IP")
	}
	rel2()

	// But the warm slot still protects a reconnect of the SAME ip.
	okSame, relSame := tr.Check("user@test", "1.1.1.1", now.Add(5*time.Second))
	if !okSame {
		t.Fatal("same-IP reconnect within TTL must be allowed (warm slot)")
	}
	relSame()

	// After the TTL the second IP is definitely allowed.
	okAfter, rel := tr.Check("user@test", "2.2.2.2", now.Add(2*defaultTTL))
	if !okAfter {
		t.Fatal("after TTL expiry a different IP must be allowed")
	}
	rel()
}

// T4: reconnect of the SAME IP is not double counted.
func TestTrackerSameIPReconnect(t *testing.T) {
	tr := newTestTracker(map[string]limitEntry{"user@test": {LimitIP: 1}})
	now := time.Now()

	// Connection 1 from the IP.
	ok1, rel1 := tr.Check("user@test", "1.1.1.1", now)
	if !ok1 {
		t.Fatal("first connection must be allowed")
	}
	// Connection 2 from the SAME IP (concurrent second device behind NAT).
	ok2, rel2 := tr.Check("user@test", "1.1.1.1", now)
	if !ok2 {
		t.Fatal("same IP reconnect must not be rejected (same slot)")
	}
	rel1()
	rel2()

	// Reconnect after brief idle (within TTL): still one slot, allowed.
	ok3, rel3 := tr.Check("user@test", "1.1.1.1", now.Add(5*time.Second))
	if !ok3 {
		t.Fatal("same-IP reconnect within TTL must be allowed")
	}
	rel3()
}

// T5: no limit configured => fail-open (always allow).
func TestTrackerFailOpen(t *testing.T) {
	tr := newTestTracker(map[string]limitEntry{}) // no limits at all
	now := time.Now()
	for i := 0; i < 10; i++ {
		ok, rel := tr.Check("stranger@test", "9.9.9.9", now)
		if !ok {
			t.Fatal("no limit configured => must allow (fail-open)")
		}
		rel()
	}

	// limitIp=0 also means unlimited (panel omits it).
	tr2 := newTestTracker(map[string]limitEntry{"user@test": {LimitIP: 0}})
	ok, rel := tr2.Check("user@test", "1.1.1.1", now)
	if !ok {
		t.Fatal("limitIp=0 must allow")
	}
	rel()

	// Disabled entry: allow.
	tr3 := newTestTracker(map[string]limitEntry{"user@test": {LimitIP: 1, Disabled: true}})
	ok3, rel3 := tr3.Check("user@test", "1.1.1.1", now)
	if !ok3 {
		t.Fatal("disabled entry must allow")
	}
	rel3()

	// Empty email / ip: allow (non-user traffic).
	tr4 := newTestTracker(map[string]limitEntry{"user@test": {LimitIP: 1}})
	ok4, rel4 := tr4.Check("", "1.1.1.1", now)
	if !ok4 {
		t.Fatal("empty email must allow")
	}
	rel4()
}

// T6: 1000 concurrent connections, no race, counts stay exact.
func TestTrackerConcurrency(t *testing.T) {
	tr := newTestTracker(map[string]limitEntry{
		"user@test": {LimitIP: 100},
	})
	now := time.Now()

	const workers = 200
	const perWorker = 20 // 4000 total attempts over 100 slots

	var wg sync.WaitGroup
	var allowedCount int64
	var mu sync.Mutex
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			ip := ipForWorker(w)
			for i := 0; i < perWorker; i++ {
				ok, rel := tr.Check("user@test", ip, now)
				if ok {
					mu.Lock()
					allowedCount++
					mu.Unlock()
					rel()
				}
			}
		}(w)
	}
	wg.Wait()

	// All 200 distinct IPs compete for 100 slots; every worker eventually
	// released, so no slot may stay live.
	for _, s := range tr.Snapshot() {
		if s.Conns != 0 {
			t.Fatalf("slot %s/%s leaked %d live conns", s.Email, s.IP, s.Conns)
		}
	}
	if allowedCount == 0 {
		t.Fatal("at least some connections must be allowed")
	}
}

func ipForWorker(w int) string {
	return string(rune('0'+w%10)) + "." + string(rune('0'+(w/10)%10)) + ".0.1"
}

// --- Limits file loading (panel → fork channel) ---------------------------

func TestLimitsFileReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "iplimits.json")

	write := func(limits map[string]limitEntry) {
		data, _ := json.Marshal(limitsFile{Limits: limits})
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		// Ensure mtime differs between writes (Windows granularity).
		time.Sleep(20 * time.Millisecond)
	}

	tr := &Tracker{
		limits:   make(map[string]limitEntry),
		active:   make(map[string]map[string]*slot),
		filePath: path,
	}

	write(map[string]limitEntry{"user@test": {LimitIP: 1}})
	tr.reload(false)
	now := time.Now()

	ok, rel := tr.Check("user@test", "1.1.1.1", now)
	if !ok {
		t.Fatal("limit from file must be enforced: first IP allowed")
	}
	if ok2, _ := tr.Check("user@test", "2.2.2.2", now); ok2 {
		t.Fatal("limit from file must be enforced: second IP rejected")
	}
	rel()

	// Panel raises the limit to 2: after reload the second IP is allowed.
	write(map[string]limitEntry{"user@test": {LimitIP: 2}})
	tr.reloadIfChanged()
	if ok3, rel3 := tr.Check("user@test", "2.2.2.2", now); !ok3 {
		t.Fatal("after limit raise to 2, second IP must be allowed")
	} else {
		rel3()
	}
}

// Malformed file: last known good limits stay in force.
func TestLimitsFileMalformedKeepsState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "iplimits.json")
	if err := os.WriteFile(path, []byte(`{not json`), 0o644); err != nil {
		t.Fatal(err)
	}

	tr := &Tracker{
		limits:   map[string]limitEntry{"user@test": {LimitIP: 1}},
		active:   make(map[string]map[string]*slot),
		filePath: path,
	}
	tr.reload(true) // malformed: must keep previous limits

	now := time.Now()
	// First IP takes the only slot.
	ok, rel := tr.Check("user@test", "1.1.1.1", now)
	if !ok {
		t.Fatal("first IP must be allowed under the kept limit")
	}
	defer rel()
	// Second distinct IP must still be rejected — proving the limit from
	// the pre-malformed state survived the failed reload.
	if ok2, _ := tr.Check("user@test", "2.2.2.2", now); ok2 {
		t.Fatal("malformed file must keep last known good limits (second IP rejected)")
	}
}

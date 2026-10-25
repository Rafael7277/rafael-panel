// client_limit_router.go implements the sticky enforcement layer of the
// per-client IP limit (design: iplimit.md).
//
// The engine (client_limit_engine.go) detects violations; this service
// makes each violation STICKY: for every (email, violating IP) it installs
// a dynamic Xray routing rule
//
//	user: [email] AND source: [ip/32] -> outboundTag "blocked" (blackhole)
//
// through the RoutingService gRPC API. Every NEW connection from a
// violating IP is then rejected at the authentication gate — the allowed
// IPs keep working without interruption.
//
// Behaviour contract:
//
//   - Rule tags are deterministic (sha256 of email|ip) so calls are
//     idempotent: re-enforcing an already-enforced pair re-creates the same
//     tag instead of piling up duplicate rules.
//   - Before adding, the same tag is removed first: Xray's RemoveRule
//     filters by tag and is a no-op for unknown tags, and AddRule APPENDS,
//     so without the pre-remove a retry after a restart would duplicate.
//   - gRPC failures are never fatal: the rule stays marked pendingAdd and
//     the next Enforce retries; the engine keeps its kick-only behaviour,
//     so the worst case is exactly the pre-router Build 39 behaviour.
//   - Rules live in Xray's memory only; a router restart (panel) or Xray
//     restart simply re-arms them on the next enforcement cycle.
package service

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/mhsanaei/3x-ui/v2/logger"
	"github.com/mhsanaei/3x-ui/v2/xray"
)

// clientLimitRulePrefix keeps the dynamic rules namespaced so they can be
// told apart from operator-written rules in Xray configs or logs.
const clientLimitRulePrefix = "rafael-ipl-"

// xrayRuleAPI is the subset of *xray.XrayAPI the router needs. Production
// passes the real API (constructed per call, same as the engine's kick
// path); tests pass a recorder implementing this interface.
type xrayRuleAPI interface {
	AddRoutingRule(ruleTag, userEmail, ip string) error
	RemoveRoutingRule(ruleTag string) error
}

// activeLimitRule is one installed (email, IP) blackhole rule.
type activeLimitRule struct {
	Email   string
	IP      string
	RuleTag string
	AddedAt time.Time

	// pendingAdd marks a rule whose gRPC AddRule failed (e.g. Xray restarted
	// without RoutingService, transient gRPC error). The next Enforce pass
	// retries it; the engine is unaffected either way.
	pendingAdd bool
}

// ClientLimitRouter tracks installed blackhole rules. It is owned by the
// client limit engine; all public methods are safe for concurrent use.
type ClientLimitRouter struct {
	mu   sync.Mutex
	rules map[string]*activeLimitRule // key: email + "|" + ip

	// dialFn builds a fresh Xray rule API client; injectable for tests.
	dialFn func() (xrayRuleAPI, error)

	// fallbackWarned avoids repeating the one-shot warning when the
	// RoutingService is unavailable.
	fallbackWarned bool
}

// newClientLimitRouter returns a router wired to the real Xray API.
func newClientLimitRouter() *ClientLimitRouter {
	return &ClientLimitRouter{
		rules: make(map[string]*activeLimitRule),
		dialFn: func() (xrayRuleAPI, error) {
			api := &xray.XrayAPI{}
			port := resolveXrayAPIPortForRouter()
			if err := api.Init(port); err != nil {
				return nil, err
			}
			return api, nil
		},
	}
}

// resolveXrayAPIPortForRouter resolves the Xray gRPC API port the same way
// the engine does. It lives in its own func so tests can stub either side.
var resolveXrayAPIPortForRouter = func() int {
	if e := clientLimitEngineInst; e != nil && e.apiPort > 0 {
		return e.apiPort
	}
	return defaultXrayAPIPort
}

// ruleKey is the map key for an (email, IP) pair.
func ruleKey(email, ip string) string {
	return email + "|" + ip
}

// ruleTagFor derives the deterministic Xray rule tag for an (email, IP)
// pair. A short hash of the pair keeps the tag unique, stable and free of
// user-controlled characters (emails appear in systemd logs and process
// listings; raw emails would also risk Xray tag-length quirks).
func ruleTagFor(email, ip string) string {
	sum := sha256.Sum256([]byte(email + "|" + ip))
	return clientLimitRulePrefix + hex.EncodeToString(sum[:])[:16]
}

// dial returns a connected rule API client, or an error the caller treats
// as "router unavailable, stay in kick-only mode".
func (r *ClientLimitRouter) dial() (xrayRuleAPI, error) {
	if r.dialFn == nil {
		return nil, fmt.Errorf("router dial function not configured")
	}
	return r.dialFn()
}

// Enforce installs (or retries) a blackhole rule for every given violating
// IP of one client. It is called from the engine right at the kick moment.
// Errors are logged, never propagated: enforcement must not fail the kick.
func (r *ClientLimitRouter) Enforce(email string, violatingIPs []string) {
	if email == "" || len(violatingIPs) == 0 {
		return
	}

	api, err := r.dial()
	if err != nil {
		r.warnFallbackOnce("cannot reach Xray API: " + err.Error())
		return
	}
	if closer, ok := api.(interface{ Close() }); ok {
		defer closer.Close()
	}

	for _, ip := range violatingIPs {
		if ip == "" {
			continue
		}
		key := ruleKey(email, ip)
		tag := ruleTagFor(email, ip)

		r.mu.Lock()
		if rule, exists := r.rules[key]; exists && !rule.pendingAdd {
			r.mu.Unlock()
			continue // already installed, nothing to do
		}
		r.rules[key] = &activeLimitRule{
			Email:     email,
			IP:        ip,
			RuleTag:   tag,
			AddedAt:   time.Now(),
			pendingAdd: true,
		}
		r.mu.Unlock()

		// Pre-remove then add: idempotent across retries and restarts
		// (AddRule appends; RemoveRule on an unknown tag is a no-op).
		if err := api.RemoveRoutingRule(tag); err != nil {
			logger.Debugf("[LIMIT_RULE] pre-remove before (re)install failed for %s: %s", tag, err)
		}
		if err := api.AddRoutingRule(tag, email, ip); err != nil {
			r.warnFallbackOnce(fmt.Sprintf("AddRoutingRule failed for %s: %s", tag, err))
			// stays pendingAdd=true; retried on the next Enforce pass
			continue
		}
		r.mu.Lock()
		if rule, ok := r.rules[key]; ok {
			rule.pendingAdd = false
		}
		r.mu.Unlock()
		logLimitRulef("[LIMIT_RULE] Email = %s || Blocked NEW IP = %s || RuleTag = %s", email, ip, tag)
	}
}

// Release removes the blackhole rule for one (email, IP) — called when the
// IP is no longer violating (TTL expiry, back under the limit, or the IP
// became one of the allowed ones).
func (r *ClientLimitRouter) Release(email, ip string) {
	if email == "" || ip == "" {
		return
	}
	key := ruleKey(email, ip)

	r.mu.Lock()
	rule, ok := r.rules[key]
	if !ok {
		r.mu.Unlock()
		return
	}
	delete(r.rules, key)
	r.mu.Unlock()

	// Even a pending (never successfully added) rule is released: the
	// pre-remove in Enforce makes Xray-side duplicates impossible, so a
	// stray RemoveRule for a tag Xray never saw is a harmless no-op.
	api, err := r.dial()
	if err != nil {
		logger.Debugf("[LIMIT_RULE] release skipped (API unreachable): %s", err)
		return
	}
	if closer, ok := api.(interface{ Close() }); ok {
		defer closer.Close()
	}
	if err := api.RemoveRoutingRule(rule.RuleTag); err != nil {
		logger.Debugf("[LIMIT_RULE] RemoveRoutingRule failed for %s: %s", rule.RuleTag, err)
		return
	}
	logLimitRulef("[LIMIT_RULE] Email = %s || Unblocked IP = %s || RuleTag = %s", email, ip, rule.RuleTag)
}

// ReleaseAll removes every rule of one client — called when the client is
// disabled, deleted, or its limit is removed.
func (r *ClientLimitRouter) ReleaseAll(email string) {
	if email == "" {
		return
	}
	r.mu.Lock()
	var tags []string
	ips := make(map[string]string) // tag -> ip (for logging)
	for key, rule := range r.rules {
		if rule.Email == email {
			tags = append(tags, rule.RuleTag)
			ips[rule.RuleTag] = rule.IP
			delete(r.rules, key)
		}
	}
	r.mu.Unlock()
	if len(tags) == 0 {
		return
	}

	api, err := r.dial()
	if err != nil {
		logger.Debugf("[LIMIT_RULE] release-all skipped (API unreachable): %s", err)
		return
	}
	if closer, ok := api.(interface{ Close() }); ok {
		defer closer.Close()
	}
	for _, tag := range tags {
		if err := api.RemoveRoutingRule(tag); err != nil {
			logger.Debugf("[LIMIT_RULE] RemoveRoutingRule failed for %s: %s", tag, err)
			continue
		}
		logLimitRulef("[LIMIT_RULE] Email = %s || Unblocked IP = %s || RuleTag = %s", email, ips[tag], tag)
	}
}

// RuleSnapshot is the exported view of one active rule (for the UI/tests).
type RuleSnapshot struct {
	Email   string    `json:"email"`
	IP      string    `json:"ip"`
	RuleTag string    `json:"ruleTag"`
	AddedAt time.Time `json:"addedAt"`
	Pending bool      `json:"pending"`
}

// Snapshot returns all currently tracked rules, sorted by email then IP.
func (r *ClientLimitRouter) Snapshot() []RuleSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]RuleSnapshot, 0, len(r.rules))
	for _, rule := range r.rules {
		out = append(out, RuleSnapshot{
			Email:   rule.Email,
			IP:      rule.IP,
			RuleTag: rule.RuleTag,
			AddedAt: rule.AddedAt,
			Pending: rule.pendingAdd,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Email != out[j].Email {
			return out[i].Email < out[j].Email
		}
		return out[i].IP < out[j].IP
	})
	return out
}

// warnFallbackOnce logs the RoutingService-unavailable warning one time so
// operators know sticky blocking is off and how to turn it on.
func (r *ClientLimitRouter) warnFallbackOnce(msg string) {
	r.mu.Lock()
	first := !r.fallbackWarned
	r.fallbackWarned = true
	r.mu.Unlock()
	logger.Warningf("[LIMIT_RULE] %s", msg)
	if first {
		logger.Warningf("[LIMIT_RULE] sticky IP blocking unavailable — the engine stays in kick-only mode; " +
			"add \"RoutingService\" to api.services in the Xray config and restart Xray to enable it")
	}
}

// logLimitRulef writes [LIMIT_RULE] lines to the same log file the engine
// uses for [LIMIT_IP] lines (3xipl.log), so operators tail one file.
func logLimitRulef(format string, args ...any) {
	logIpFile, err := os.OpenFile(xray.GetIPLimitLogPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		logger.Warningf("[LIMIT_RULE] failed to open IP limit log file: %s", err)
		return
	}
	defer logIpFile.Close()
	l := log.New(logIpFile, "", log.LstdFlags)
	l.Printf(format, args...)
}

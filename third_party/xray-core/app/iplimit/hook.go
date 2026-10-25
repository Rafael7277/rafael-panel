// hook.go is the RAFAEL:IPLIMIT phase-2 integration point (iplimit.md §15).
//
// This file holds the logic invoked from a marked 4-line insertion at the
// top of DefaultDispatcher.routedDispatch (app/dispatcher/default.go) —
// the single convergence point of all proxy protocols (VLESS DispatchLink,
// VMess / Trojan / Shadowsocks Dispatch), where session.Inbound.User and
// session.Inbound.Source are already populated and no user byte has been
// relayed yet.
package iplimit

import (
	"context"
	"errors"
	"time"

	"github.com/xtls/xray-core/common/session"
)

// ErrIPLimitReject is returned to the inbound proxy when the source IP is
// over the client's limit. It mirrors the standard "invalid user" style of
// rejection so client behavior (retry logic, error surfaces) stays normal
// and the gate adds no distinctive fingerprint beyond the rejection itself.
var ErrIPLimitReject = errors.New("rejected: user is already connected from the maximum number of devices")

// CheckIn evaluates the IP limit for a connection about to be dispatched.
// It returns nil when the connection may proceed; the second return value
// is the release function the caller MUST call (via defer) when the
// dispatch ends.
//
// Traffic without a user identity (dokodemo/API/health checks) is allowed.
// Panel-managed clients must have an explicit policy entry; the tracker
// rejects unknown clients if the policy snapshot is strict or unavailable.
func CheckIn(ctx context.Context) (error, func()) {
	inbound := session.InboundFromContext(ctx)
	if inbound == nil || inbound.User == nil || inbound.User.Email == "" {
		return nil, func() {}
	}
	ip := ""
	if inbound.Source.IsValid() && inbound.Source.Address.Family().IsIP() {
		ip = inbound.Source.Address.IP().String()
	}

	tracker := Global()
	allowed, release := tracker.Check(inbound.User.Email, ip, time.Now())
	if !allowed {
		return ErrIPLimitReject, nil
	}
	return nil, release
}

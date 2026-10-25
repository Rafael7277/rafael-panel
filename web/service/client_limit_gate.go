// client_limit_gate.go is the panel side of the RAFAEL:IPLIMIT phase-2
// gate (design: iplimit.md §16).
//
// The forked Xray binary reads per-client IP limits from a JSON file whose
// path is passed via the XRAY_IPLIMIT_FILE environment variable (see
// forks/xray-core/app/iplimit). This service keeps that file in sync with
// the database:
//
//   - RefreshFromDB() writes a strict snapshot for every client of every
//     inbound, including explicit unlimited and disabled entries.
//   - It is called on panel start and after every client save/disable/
//     delete through the existing notify hooks (the same places that call
//     clientLimitEngineNotifyClientUpdated).
//
// The panel does not restart Xray for a limit change: the fork re-reads the
// file for each new connection, with a 10s fallback ticker. The old log-based
// engine only retains recent IP observations for display in production.
package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/logger"
	"github.com/mhsanaei/3x-ui/v2/xray"
)

// GateLimitEntry is one client's gate limit as consumed by the fork
// (app/iplimit limitEntry — field names must stay in sync).
type GateLimitEntry struct {
	LimitIP  int64 `json:"limitIp"`
	Disabled bool  `json:"disabled,omitempty"`
}

// gateLimitsFile is the JSON shape of iplimits.json.
type gateLimitsFile struct {
	Strict bool                      `json:"strict"`
	Limits map[string]GateLimitEntry `json:"limits"`
}

// ClientLimitGate owns the limits file write path.
type ClientLimitGate struct {
	mu sync.Mutex
	// lastWrite tracks the JSON bytes of the last successful write so
	// no-op refreshes do not touch the file (and Xray's mtime watcher).
	lastWrite []byte
}

var clientLimitGate = &ClientLimitGate{}

// GetIPLimitsFilePath returns the path of the gate limits file next to the
// panel database (inside the DB folder so flat and release installs both
// keep it under their own /usr/local/rafael tree).
func GetIPLimitsFilePath() string {
	return xray.GetIPLimitConfigPath()
}

// RefreshFromDB rebuilds the limits file from every inbound's clients.
// Every client with an email gets an explicit entry, including unlimited
// clients. Strict mode prevents an unknown client from bypassing policy.
// Disabled entries reject new sessions. There is no user-configurable grace.
func (g *ClientLimitGate) RefreshFromDB() error {
	g.mu.Lock()
	defer g.mu.Unlock()

	limits := make(map[string]GateLimitEntry)

	db := database.GetDB()
	if db == nil {
		return fmt.Errorf("database is not initialized")
	}
	var inbounds []model.Inbound
	if err := db.Model(model.Inbound{}).Find(&inbounds).Error; err != nil {
		return err
	}
	// Quota and expiry enforcement is recorded in client_traffics as well as
	// inbound JSON. Xray's config builder consults this table, so the live
	// admission gate must honor a disabled stats row even if a stale inbound
	// client object still says enable=true.
	var clientStats []xray.ClientTraffic
	if err := db.Model(&xray.ClientTraffic{}).Select("email", "enable").Find(&clientStats).Error; err != nil {
		return err
	}
	disabledByTraffic := make(map[string]struct{})
	for _, stat := range clientStats {
		if !stat.Enable && strings.TrimSpace(stat.Email) != "" {
			disabledByTraffic[strings.ToLower(strings.TrimSpace(stat.Email))] = struct{}{}
		}
	}

	for _, inbound := range inbounds {
		var settings struct {
			Clients []model.Client `json:"clients"`
		}
		// Ignore unrelated top-level settings, but do not silently drop the
		// limits for an inbound whose clients could not be decoded. Returning
		// before writing leaves the last known-good gate file in place.
		if err := json.Unmarshal([]byte(inbound.Settings), &settings); err != nil {
			return fmt.Errorf("invalid client settings for inbound %d: %w", inbound.Id, err)
		}
		for _, client := range settings.Clients {
			if client.Email == "" {
				if client.LimitIP > 0 {
					return fmt.Errorf("limited client in inbound %d has no email identity", inbound.Id)
				}
				continue
			}
			if client.LimitIP < 0 {
				return fmt.Errorf("invalid negative IP limit for client %q in inbound %d", client.Email, inbound.Id)
			}
			entry := GateLimitEntry{LimitIP: int64(client.LimitIP)}
			_, trafficDisabled := disabledByTraffic[strings.ToLower(strings.TrimSpace(client.Email))]
			if !client.Enable || !inbound.Enable || trafficDisabled {
				entry.Disabled = true
			}
			// Same email in two inbounds: enabled entries win over disabled
			// entries, and among enabled entries the strictest positive limit wins.
			if prev, ok := limits[client.Email]; ok {
				switch {
				case !prev.Disabled && entry.Disabled:
					continue
				case prev.Disabled && !entry.Disabled:
					// replace disabled with enabled entry
				case !prev.Disabled && !entry.Disabled && prev.LimitIP > 0 &&
					(entry.LimitIP == 0 || prev.LimitIP <= entry.LimitIP):
					continue
				case !prev.Disabled && !entry.Disabled && entry.LimitIP == 0 && prev.LimitIP == 0:
					continue
				}
			}
			limits[client.Email] = entry
		}
	}

	data, err := json.MarshalIndent(gateLimitsFile{Strict: true, Limits: limits}, "", "  ")
	if err != nil {
		return err
	}

	path := GetIPLimitsFilePath()
	// No-op guard: identical content must not bump the mtime. Also verify the
	// file itself, so deleting or replacing it cannot leave a stale in-memory
	// lastWrite value that suppresses the startup policy write.
	if bytes.Equal(data, g.lastWrite) {
		if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, data) {
			return nil
		}
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".iplimits-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	g.lastWrite = data
	logger.Debugf("[LIMIT_GATE] wrote iplimits.json: %d clients with policy entries", len(limits))
	return nil
}

// ClientLimitGateRefresh refreshes the fork gate limits file from the DB.
// Public entry point for notify hooks and panel startup. If a complete policy
// snapshot cannot be published, the running Xray process is stopped so a
// stale, permissive limit cannot remain in service; the restart job retries.
func ClientLimitGateRefresh() {
	if err := clientLimitGate.RefreshFromDB(); err != nil {
		failClosedXrayForIPLimit(err)
	}
}

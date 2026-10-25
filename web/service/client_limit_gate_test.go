package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/xray"
)

// gateLimitsOf reads and parses the gate limits file for assertions.
func gateLimitsOf(t *testing.T) map[string]GateLimitEntry {
	t.Helper()
	path := xray.GetIPLimitConfigPath()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read iplimits.json: %v", err)
	}
	var lf gateLimitsFile
	if err := json.Unmarshal(data, &lf); err != nil {
		t.Fatalf("parse iplimits.json: %v", err)
	}
	return lf.Limits
}

// T-G1: refresh writes exactly the limited clients with the right values.
func TestClientLimitGateRefreshWritesLimitedClients(t *testing.T) {
	initLifecycleTestDB(t)
	createLimitInbound(t, []model.Client{
		{ID: "uuid-a", Email: "limited@test", LimitIP: 2, LimitGrace: 30, Enable: true},
		{ID: "uuid-b", Email: "unlimited@test", LimitIP: 0, Enable: true},
		{ID: "uuid-c", Email: "disabled@test", LimitIP: 1, Enable: false},
	})

	if err := clientLimitGate.RefreshFromDB(); err != nil {
		t.Fatalf("RefreshFromDB: %v", err)
	}

	limits := gateLimitsOf(t)
	if got, ok := limits["limited@test"]; !ok || got.LimitIP != 2 {
		t.Fatalf("limited@test entry = %+v, want {LimitIP:2}", got)
	}
	// Unlimited clients still need an explicit entry in strict mode.
	if got, ok := limits["unlimited@test"]; !ok || got.LimitIP != 0 {
		t.Fatalf("unlimited@test entry = %+v, want {LimitIP:0}", got)
	}
	// Disabled client: explicit disabled entry in the strict snapshot.
	if got, ok := limits["disabled@test"]; !ok || !got.Disabled {
		t.Fatalf("disabled@test entry = %+v, want an explicit disabled entry", got)
	}
}

// T-G2: notify hook refreshes the file after a client edit.
func TestClientLimitGateRefreshOnNotify(t *testing.T) {
	initLifecycleTestDB(t)
	inbound := createLimitInbound(t, []model.Client{
		{ID: "uuid-a", Email: "user@test", LimitIP: 1, Enable: true},
	})

	if err := clientLimitGate.RefreshFromDB(); err != nil {
		t.Fatalf("RefreshFromDB: %v", err)
	}
	if got := gateLimitsOf(t)["user@test"].LimitIP; got != 1 {
		t.Fatalf("initial limit = %d, want 1", got)
	}

	// Operator raises the limit to 3 in the DB, then the save-path notify
	// hook fires.
	var settings struct {
		Clients []model.Client `json:"clients"`
	}
	_ = json.Unmarshal([]byte(inbound.Settings), &settings)
	settings.Clients[0].LimitIP = 3
	body, _ := json.Marshal(map[string]any{"clients": settings.Clients})
	if err := database.GetDB().Model(model.Inbound{}).Where("id = ?", inbound.Id).
		Update("settings", string(body)).Error; err != nil {
		t.Fatalf("update settings: %v", err)
	}
	clientLimitEngineNotifyClientUpdated("user@test", "user@test")

	if got := gateLimitsOf(t)["user@test"].LimitIP; got != 3 {
		t.Fatalf("limit after notify = %d, want 3 (file must refresh)", got)
	}
}

// T-G3: no-op refresh does not rewrite the file (mtime stable).
func TestClientLimitGateNoOpRefreshKeepsFile(t *testing.T) {
	initLifecycleTestDB(t)
	createLimitInbound(t, []model.Client{
		{ID: "uuid-a", Email: "user@test", LimitIP: 1, Enable: true},
	})

	if err := clientLimitGate.RefreshFromDB(); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	fi, err := os.Stat(xray.GetIPLimitConfigPath())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	// Second refresh with unchanged DB: no write, same mtime.
	if err := clientLimitGate.RefreshFromDB(); err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	fi2, err := os.Stat(xray.GetIPLimitConfigPath())
	if err != nil {
		t.Fatalf("stat2: %v", err)
	}
	if !fi.ModTime().Equal(fi2.ModTime()) {
		t.Fatal("no-op refresh must not rewrite the file")
	}
}

// T-G4: the limits file lands in the DB folder (co-located with the state
// the fork needs to survive restarts).
func TestClientLimitGateFilePath(t *testing.T) {
	initLifecycleTestDB(t)
	path := xray.GetIPLimitConfigPath()
	if filepath.Base(path) != "iplimits.json" {
		t.Fatalf("gate file base name = %q, want iplimits.json", filepath.Base(path))
	}
}

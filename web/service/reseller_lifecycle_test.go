package service

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/logger"
	"github.com/mhsanaei/3x-ui/v2/xray"
	"github.com/op/go-logging"
)

// TestMain initializes the package logger so code paths that log (e.g. the
// Xray API fallbacks in UpdateInboundClient) do not hit a nil logger in tests.
func TestMain(m *testing.M) {
	logDir, err := os.MkdirTemp("", "xui-service-test-log-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "mkdir temp log dir: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(logDir)
	_ = os.Setenv("XUI_LOG_FOLDER", logDir)
	logger.InitLogger(logging.CRITICAL)
	code := m.Run()
	logger.CloseLogger()
	os.Exit(code)
}

func initLifecycleTestDB(t *testing.T) {
	t.Helper()
	dbDir := t.TempDir()
	t.Setenv("XUI_DB_FOLDER", dbDir)
	if err := database.InitDB(filepath.Join(dbDir, "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { _ = database.CloseDB() })
}

func createAdminAndReseller(t *testing.T, maxTraffic int64) (*model.User, *model.User) {
	t.Helper()
	db := database.GetDB()
	admin := &model.User{Username: "lifecycle-admin", Password: "x", Role: UserRoleAdmin, Enabled: true}
	if err := db.Create(admin).Error; err != nil {
		t.Fatalf("create admin: %v", err)
	}
	parent := admin.Id
	reseller := &model.User{
		Username:     "lifecycle-reseller",
		Password:     "x",
		Role:         UserRoleReseller,
		ParentUserID: &parent,
		Enabled:      true,
		MaxTraffic:   maxTraffic,
	}
	if err := db.Create(reseller).Error; err != nil {
		t.Fatalf("create reseller: %v", err)
	}
	return admin, reseller
}

func createInboundWithClients(t *testing.T, userId int, tag string, port int, protocol model.Protocol, clients []model.Client, extra map[string]any) *model.Inbound {
	t.Helper()
	db := database.GetDB()
	settings := map[string]any{}
	for k, v := range extra {
		settings[k] = v
	}
	settings["clients"] = clients
	body, err := json.Marshal(settings)
	if err != nil {
		t.Fatalf("marshal settings: %v", err)
	}
	inbound := &model.Inbound{
		UserId:         userId,
		Tag:            tag,
		Enable:         true,
		Port:           port,
		Protocol:       protocol,
		StreamSettings: `{"network":"tcp","security":"reality"}`,
		Settings:       string(body),
	}
	if err := db.Create(inbound).Error; err != nil {
		t.Fatalf("create inbound: %v", err)
	}
	svc := &InboundService{}
	for i := range clients {
		if err := svc.AddClientStat(db, inbound.Id, &clients[i]); err != nil {
			t.Fatalf("AddClientStat(%s): %v", clients[i].Email, err)
		}
	}
	return inbound
}

func reloadInboundClients(t *testing.T, inboundId int) []model.Client {
	t.Helper()
	inbound, err := (&InboundService{}).GetInbound(inboundId)
	if err != nil {
		t.Fatalf("GetInbound: %v", err)
	}
	clients, err := (&InboundService{}).GetClients(inbound)
	if err != nil {
		t.Fatalf("GetClients: %v", err)
	}
	return clients
}

func findClientByEmail(t *testing.T, clients []model.Client, email string) *model.Client {
	t.Helper()
	for i := range clients {
		if clients[i].Email == email {
			return &clients[i]
		}
	}
	t.Fatalf("client %q not found in %#v", email, clients)
	return nil
}

func reloadTrafficRow(t *testing.T, email string) *xray.ClientTraffic {
	t.Helper()
	row := &xray.ClientTraffic{}
	if err := database.GetDB().Model(xray.ClientTraffic{}).Where("email = ?", email).First(row).Error; err != nil {
		t.Fatalf("reload client_traffics for %q: %v", email, err)
	}
	return row
}

func setClientUsage(t *testing.T, email string, up, down int64, enable bool) {
	t.Helper()
	if err := database.GetDB().Model(xray.ClientTraffic{}).Where("email = ?", email).Updates(map[string]any{
		"up":     up,
		"down":   down,
		"enable": enable,
	}).Error; err != nil {
		t.Fatalf("set usage for %q: %v", email, err)
	}
}

func reloadReseller(t *testing.T, id int) *model.User {
	t.Helper()
	user, err := (&UserService{}).GetUserByID(id)
	if err != nil {
		t.Fatalf("reload user %d: %v", id, err)
	}
	return user
}

// P1 regression: enabling/disabling a reseller's clients must preserve the
// protocol-level keys of the inbound settings (shadowsocks method/password/
// network/ivCheck), and must not touch clients the reseller does not own.
func TestSetUserRuntimeEnabled_PreservesProtocolSettings(t *testing.T) {
	initLifecycleTestDB(t)
	db := database.GetDB()
	_, reseller := createAdminAndReseller(t, 0)

	owned := model.Client{Email: "own-client", Password: "pw1", Enable: true, OwnerUserID: reseller.Id}
	other := model.Client{Email: "other-client", Password: "pw2", Enable: true}
	settings := map[string]any{
		"method":   "2022-blake3-aes-128-gcm",
		"password": "server-password",
		"network":  "tcp,udp",
		"ivCheck":  true,
		"clients":  []model.Client{owned, other},
	}
	body, err := json.Marshal(settings)
	if err != nil {
		t.Fatalf("marshal settings: %v", err)
	}
	inbound := &model.Inbound{
		UserId: reseller.Id, Tag: "ss-preserve", Enable: true, Port: 41001,
		Protocol: model.Shadowsocks, Settings: string(body),
	}
	if err := db.Create(inbound).Error; err != nil {
		t.Fatalf("create inbound: %v", err)
	}

	svc := &InboundService{}
	changed, err := svc.SetUserRuntimeEnabled(reseller.Id, false)
	if err != nil {
		t.Fatalf("SetUserRuntimeEnabled: %v", err)
	}
	if !changed {
		t.Fatalf("expected changed=true on first disable")
	}

	reloaded, err := svc.GetInbound(inbound.Id)
	if err != nil {
		t.Fatalf("GetInbound: %v", err)
	}
	var saved map[string]any
	if err := json.Unmarshal([]byte(reloaded.Settings), &saved); err != nil {
		t.Fatalf("unmarshal saved settings: %v", err)
	}
	if saved["method"] != "2022-blake3-aes-128-gcm" {
		t.Errorf("protocol key method lost: %#v", saved)
	}
	if saved["password"] != "server-password" {
		t.Errorf("protocol key password lost: %#v", saved)
	}
	if saved["network"] != "tcp,udp" {
		t.Errorf("protocol key network lost: %#v", saved)
	}
	if v, ok := saved["ivCheck"].(bool); !ok || !v {
		t.Errorf("protocol key ivCheck lost: %#v", saved)
	}
	if !reloaded.Enable {
		t.Errorf("reseller runtime toggle must not disable the inbound itself")
	}

	clients := reloadInboundClients(t, inbound.Id)
	if c := findClientByEmail(t, clients, "own-client"); c.Enable {
		t.Errorf("owned client should be disabled")
	}
	if c := findClientByEmail(t, clients, "other-client"); !c.Enable {
		t.Errorf("non-owned client must stay enabled")
	}

	// Idempotency: a second sync must not rewrite anything.
	before := reloaded.Settings
	changed, err = svc.SetUserRuntimeEnabled(reseller.Id, false)
	if err != nil {
		t.Fatalf("second SetUserRuntimeEnabled: %v", err)
	}
	if changed {
		t.Errorf("second disable should report changed=false")
	}
	reloaded2, err := svc.GetInbound(inbound.Id)
	if err != nil {
		t.Fatalf("GetInbound: %v", err)
	}
	if reloaded2.Settings != before {
		t.Errorf("settings rewritten on no-op sync")
	}
}

// P2 regression: lifecycle action "none" must leave the reseller's clients
// untouched, including on the recurring runtime sync (traffic job).
func TestResellerLifecycle_DisableActionNone_LeavesClientsEnabled(t *testing.T) {
	initLifecycleTestDB(t)
	admin, reseller := createAdminAndReseller(t, 0)

	a := model.Client{ID: "11111111-1111-4111-8111-111111111111", Email: "a@t", Enable: true, OwnerUserID: reseller.Id}
	b := model.Client{ID: "22222222-2222-4222-8222-222222222222", Email: "b@t", Enable: true, OwnerUserID: reseller.Id}
	inbound := createInboundWithClients(t, reseller.Id, "vless-none", 41002, model.VLESS, []model.Client{a, b}, map[string]any{"decryption": "none"})

	us := &UserService{}
	if _, err := us.UpdateResellerWithOptions(admin.Id, reseller.Id, reseller.Username, "", 10, 0, false,
		ResellerPermissions{}, nil, ResellerUpdateOptions{ClientAction: ResellerLifecycleNone}); err != nil {
		t.Fatalf("disable with action none: %v", err)
	}

	clients := reloadInboundClients(t, inbound.Id)
	if c := findClientByEmail(t, clients, "a@t"); !c.Enable {
		t.Errorf("action none must not disable clients")
	}
	if c := findClientByEmail(t, clients, "b@t"); !c.Enable {
		t.Errorf("action none must not disable clients")
	}

	// The recurring sync (traffic job) must not force-disable either.
	if err := us.SyncResellerRuntimeState(reseller.Id); err != nil {
		t.Fatalf("SyncResellerRuntimeState: %v", err)
	}
	clients = reloadInboundClients(t, inbound.Id)
	if c := findClientByEmail(t, clients, "a@t"); !c.Enable {
		t.Errorf("runtime sync must not disable clients for action none")
	}

	if state := parseManagedState(reloadReseller(t, reseller.Id).ManagedState); len(state.AutoDisabledClientKeys) != 0 {
		t.Errorf("action none must not record managed state: %#v", state)
	}
}

// P2 regression: "disable_clients" disables and records exactly the enabled
// owned clients; re-enabling the reseller restores them, while a client that
// was already manually disabled stays disabled.
func TestResellerLifecycle_DisableClientsRecordsAndRestores(t *testing.T) {
	initLifecycleTestDB(t)
	admin, reseller := createAdminAndReseller(t, 0)

	a := model.Client{ID: "11111111-1111-4111-8111-111111111111", Email: "a@t", Enable: true, OwnerUserID: reseller.Id}
	b := model.Client{ID: "22222222-2222-4222-8222-222222222222", Email: "b@t", Enable: false, OwnerUserID: reseller.Id}
	inbound := createInboundWithClients(t, reseller.Id, "vless-disable", 41003, model.VLESS, []model.Client{a, b}, map[string]any{"decryption": "none"})

	us := &UserService{}
	if _, err := us.UpdateResellerWithOptions(admin.Id, reseller.Id, reseller.Username, "", 10, 0, false,
		ResellerPermissions{}, nil, ResellerUpdateOptions{ClientAction: ResellerLifecycleDisable}); err != nil {
		t.Fatalf("disable with action disable_clients: %v", err)
	}

	clients := reloadInboundClients(t, inbound.Id)
	if c := findClientByEmail(t, clients, "a@t"); c.Enable {
		t.Errorf("enabled client should be disabled by disable_clients")
	}

	state := parseManagedState(reloadReseller(t, reseller.Id).ManagedState)
	wantKey := resellerClientStateKey(inbound.Id, "a@t")
	if len(state.AutoDisabledClientKeys) != 1 || state.AutoDisabledClientKeys[0] != wantKey {
		t.Fatalf("managed state = %#v, want [%s]", state.AutoDisabledClientKeys, wantKey)
	}

	// Re-enable the reseller: recorded client restored, manually disabled one stays.
	if _, err := us.UpdateResellerWithOptions(admin.Id, reseller.Id, reseller.Username, "", 10, 0, true,
		ResellerPermissions{}, nil, ResellerUpdateOptions{ClientAction: ResellerLifecycleNone}); err != nil {
		t.Fatalf("re-enable reseller: %v", err)
	}
	clients = reloadInboundClients(t, inbound.Id)
	if c := findClientByEmail(t, clients, "a@t"); !c.Enable {
		t.Errorf("recorded client should be restored on re-enable")
	}
	if row := reloadTrafficRow(t, "a@t"); !row.Enable {
		t.Errorf("client_traffics row should be re-enabled together with the settings client")
	}
	if c := findClientByEmail(t, clients, "b@t"); c.Enable {
		t.Errorf("manually disabled client must stay disabled")
	}

	// Repeated syncs must be stable: no re-disable, no state churn.
	for i := 0; i < 3; i++ {
		if err := us.SyncResellerRuntimeState(reseller.Id); err != nil {
			t.Fatalf("repeat sync %d: %v", i, err)
		}
	}
	clients = reloadInboundClients(t, inbound.Id)
	if c := findClientByEmail(t, clients, "a@t"); !c.Enable {
		t.Errorf("repeated syncs must not re-disable restored client")
	}
	if state := parseManagedState(reloadReseller(t, reseller.Id).ManagedState); len(state.AutoDisabledClientKeys) != 0 {
		t.Errorf("managed state should be empty after restore: %#v", state)
	}
}

// P5 regression: quota exhaustion disables (and records) the reseller's
// clients; raising maxTraffic restores them; repeated ticks do not duplicate
// or corrupt the recorded state.
func TestSyncResellerRuntimeState_QuotaDisablesAndRestoresOnQuotaRaise(t *testing.T) {
	initLifecycleTestDB(t)
	db := database.GetDB()
	_, reseller := createAdminAndReseller(t, 1000)

	a := model.Client{ID: "11111111-1111-4111-8111-111111111111", Email: "a@t", Enable: true, TotalGB: 100000, OwnerUserID: reseller.Id}
	inbound := createInboundWithClients(t, reseller.Id, "vless-quota", 41004, model.VLESS, []model.Client{a}, map[string]any{"decryption": "none"})
	setClientUsage(t, "a@t", 600, 400, true) // usage == maxTraffic -> over quota

	us := &UserService{}
	if err := us.SyncResellerRuntimeState(reseller.Id); err != nil {
		t.Fatalf("sync over quota: %v", err)
	}

	if c := findClientByEmail(t, reloadInboundClients(t, inbound.Id), "a@t"); c.Enable {
		t.Fatalf("over-quota client should be disabled")
	}
	wantKey := resellerClientStateKey(inbound.Id, "a@t")
	state := parseManagedState(reloadReseller(t, reseller.Id).ManagedState)
	if len(state.AutoDisabledClientKeys) != 1 || state.AutoDisabledClientKeys[0] != wantKey {
		t.Fatalf("managed state after quota disable = %#v, want [%s]", state.AutoDisabledClientKeys, wantKey)
	}

	// Repeated ticks while over quota: state stays stable, no duplicates.
	if err := us.SyncResellerRuntimeState(reseller.Id); err != nil {
		t.Fatalf("repeat sync: %v", err)
	}
	if err := us.SyncResellerRuntimeState(reseller.Id); err != nil {
		t.Fatalf("repeat sync: %v", err)
	}
	state = parseManagedState(reloadReseller(t, reseller.Id).ManagedState)
	if len(state.AutoDisabledClientKeys) != 1 || state.AutoDisabledClientKeys[0] != wantKey {
		t.Fatalf("managed state not stable across ticks: %#v", state.AutoDisabledClientKeys)
	}
	if c := findClientByEmail(t, reloadInboundClients(t, inbound.Id), "a@t"); c.Enable {
		t.Errorf("client must stay disabled while over quota")
	}

	// Raise the quota: the client reactivates on the next sync.
	if err := db.Model(model.User{}).Where("id = ?", reseller.Id).Update("max_traffic", 100000).Error; err != nil {
		t.Fatalf("raise quota: %v", err)
	}
	if err := us.SyncResellerRuntimeState(reseller.Id); err != nil {
		t.Fatalf("sync after quota raise: %v", err)
	}
	if c := findClientByEmail(t, reloadInboundClients(t, inbound.Id), "a@t"); !c.Enable {
		t.Errorf("client should be restored after quota raise")
	}
	if row := reloadTrafficRow(t, "a@t"); !row.Enable {
		t.Errorf("client_traffics row should converge with settings client")
	}
}

// P5 regression: with the reseller disabled but under quota, a manually
// enabled client must not be undone by the recurring sync.
func TestSyncResellerRuntimeState_DisabledUnderQuotaKeepsManualEnable(t *testing.T) {
	initLifecycleTestDB(t)
	db := database.GetDB()
	admin, reseller := createAdminAndReseller(t, 100000)

	a := model.Client{ID: "11111111-1111-4111-8111-111111111111", Email: "a@t", Enable: true, OwnerUserID: reseller.Id}
	inbound := createInboundWithClients(t, reseller.Id, "vless-manual", 41005, model.VLESS, []model.Client{a}, map[string]any{"decryption": "none"})

	us := &UserService{}
	if _, err := us.UpdateResellerWithOptions(admin.Id, reseller.Id, reseller.Username, "", 10, 0, false,
		ResellerPermissions{}, nil, ResellerUpdateOptions{ClientAction: ResellerLifecycleDisable}); err != nil {
		t.Fatalf("disable reseller: %v", err)
	}
	if c := findClientByEmail(t, reloadInboundClients(t, inbound.Id), "a@t"); c.Enable {
		t.Fatalf("client should be disabled after disable_clients")
	}

	// Admin manually re-enables the client (settings + stats) while the
	// reseller is still disabled but under quota.
	clients := reloadInboundClients(t, inbound.Id)
	clients[0].Enable = true
	body, err := json.Marshal(map[string]any{"clients": clients})
	if err != nil {
		t.Fatalf("marshal settings: %v", err)
	}
	if err := db.Model(model.Inbound{}).Where("id = ?", inbound.Id).Update("settings", string(body)).Error; err != nil {
		t.Fatalf("manual enable settings: %v", err)
	}
	if err := db.Model(xray.ClientTraffic{}).Where("email = ?", "a@t").Update("enable", true).Error; err != nil {
		t.Fatalf("manual enable stats: %v", err)
	}

	for i := 0; i < 2; i++ {
		if err := us.SyncResellerRuntimeState(reseller.Id); err != nil {
			t.Fatalf("sync %d: %v", i, err)
		}
	}
	if c := findClientByEmail(t, reloadInboundClients(t, inbound.Id), "a@t"); !c.Enable {
		t.Errorf("manual enable must not be undone by the sync")
	}
}

// P3 regression: a client auto-disabled because its volume was depleted is
// re-enabled (settings + stats) when the volume is increased, even though the
// edit payload still carries enable=false from the round-trip.
func TestUpdateInboundClient_ReEnablesDepletedClientOnVolumeIncrease(t *testing.T) {
	initLifecycleTestDB(t)

	client := model.Client{
		ID: "11111111-1111-4111-8111-111111111111", Email: "depleted@t",
		Enable: true, TotalGB: 1000,
	}
	inbound := createInboundWithClients(t, 1, "vless-depleted", 41006, model.VLESS, []model.Client{client}, map[string]any{"decryption": "none"})
	// Simulate the traffic job: usage reached the limit and the client was disabled.
	setClientUsage(t, "depleted@t", 600, 400, false)
	clients := reloadInboundClients(t, inbound.Id)
	clients[0].Enable = false
	body, err := json.Marshal(map[string]any{"clients": clients})
	if err != nil {
		t.Fatalf("marshal settings: %v", err)
	}
	if err := database.GetDB().Model(model.Inbound{}).Where("id = ?", inbound.Id).Update("settings", string(body)).Error; err != nil {
		t.Fatalf("disable in settings: %v", err)
	}

	// Edit payload from the modal: volume raised, enable=false round-tripped.
	payload := client
	payload.TotalGB = 100000
	payload.Enable = false
	svc := &InboundService{}
	needRestart, err := svc.UpdateInboundClient(&model.Inbound{
		Id:       inbound.Id,
		Settings: clientsSettings(t, []model.Client{payload}),
	}, payload.ID)
	if err != nil {
		t.Fatalf("UpdateInboundClient: %v", err)
	}
	if !needRestart {
		t.Errorf("expected needRestart so the user is re-added to the Xray runtime")
	}

	if c := findClientByEmail(t, reloadInboundClients(t, inbound.Id), "depleted@t"); !c.Enable {
		t.Errorf("depleted client should be re-enabled after volume increase")
	}
	if row := reloadTrafficRow(t, "depleted@t"); !row.Enable || row.Total != 100000 {
		t.Errorf("client_traffics not converged: enable=%v total=%d", row.Enable, row.Total)
	}
}

// P3 regression: an expired client is re-enabled when the expiry is extended.
func TestUpdateInboundClient_ReEnablesExpiredClientOnExpiryExtension(t *testing.T) {
	initLifecycleTestDB(t)

	past := time.Now().UnixMilli() - 86400000
	client := model.Client{
		ID: "22222222-2222-4222-8222-222222222222", Email: "expired@t",
		Enable: true, ExpiryTime: past,
	}
	inbound := createInboundWithClients(t, 1, "vless-expired", 41007, model.VLESS, []model.Client{client}, map[string]any{"decryption": "none"})
	setClientUsage(t, "expired@t", 0, 0, false)
	clients := reloadInboundClients(t, inbound.Id)
	clients[0].Enable = false
	body, err := json.Marshal(map[string]any{"clients": clients})
	if err != nil {
		t.Fatalf("marshal settings: %v", err)
	}
	if err := database.GetDB().Model(model.Inbound{}).Where("id = ?", inbound.Id).Update("settings", string(body)).Error; err != nil {
		t.Fatalf("disable in settings: %v", err)
	}

	payload := client
	payload.ExpiryTime = time.Now().UnixMilli() + 30*86400000
	payload.Enable = false
	if _, err := (&InboundService{}).UpdateInboundClient(&model.Inbound{
		Id:       inbound.Id,
		Settings: clientsSettings(t, []model.Client{payload}),
	}, payload.ID); err != nil {
		t.Fatalf("UpdateInboundClient: %v", err)
	}

	if c := findClientByEmail(t, reloadInboundClients(t, inbound.Id), "expired@t"); !c.Enable {
		t.Errorf("expired client should be re-enabled after expiry extension")
	}
	if row := reloadTrafficRow(t, "expired@t"); !row.Enable {
		t.Errorf("client_traffics row should be re-enabled")
	}
}

// P3 regression: a client disabled manually (its limits were never exhausted)
// must stay disabled when edited.
func TestUpdateInboundClient_ManuallyDisabledClientStaysDisabled(t *testing.T) {
	initLifecycleTestDB(t)

	future := time.Now().UnixMilli() + 30*86400000
	client := model.Client{
		ID: "33333333-3333-4333-8333-333333333333", Email: "manual@t",
		Enable: true, TotalGB: 100000, ExpiryTime: future,
	}
	inbound := createInboundWithClients(t, 1, "vless-manual-disabled", 41008, model.VLESS, []model.Client{client}, map[string]any{"decryption": "none"})
	setClientUsage(t, "manual@t", 100, 100, false) // far below the limit
	clients := reloadInboundClients(t, inbound.Id)
	clients[0].Enable = false
	body, err := json.Marshal(map[string]any{"clients": clients})
	if err != nil {
		t.Fatalf("marshal settings: %v", err)
	}
	if err := database.GetDB().Model(model.Inbound{}).Where("id = ?", inbound.Id).Update("settings", string(body)).Error; err != nil {
		t.Fatalf("disable in settings: %v", err)
	}

	payload := client
	payload.Enable = false
	if _, err := (&InboundService{}).UpdateInboundClient(&model.Inbound{
		Id:       inbound.Id,
		Settings: clientsSettings(t, []model.Client{payload}),
	}, payload.ID); err != nil {
		t.Fatalf("UpdateInboundClient: %v", err)
	}

	if c := findClientByEmail(t, reloadInboundClients(t, inbound.Id), "manual@t"); c.Enable {
		t.Errorf("manually disabled client must stay disabled")
	}
	if row := reloadTrafficRow(t, "manual@t"); row.Enable {
		t.Errorf("client_traffics row must stay disabled")
	}
}

// P7 regression: client_traffics lookups must match emails case-insensitively,
// consistent with disableInvalidClients' LOWER(email) matching.
func TestUpdateAndDeleteClientStat_MatchEmailCaseInsensitive(t *testing.T) {
	initLifecycleTestDB(t)
	db := database.GetDB()

	row := &xray.ClientTraffic{InboundId: 1, Email: "User@Example.COM", Enable: false, Total: 1}
	if err := db.Create(row).Error; err != nil {
		t.Fatalf("create client_traffics: %v", err)
	}

	svc := &InboundService{}
	if err := svc.UpdateClientStat(db, "user@example.com", &model.Client{
		Email: "user@example.com", Enable: true, TotalGB: 5,
	}); err != nil {
		t.Fatalf("UpdateClientStat: %v", err)
	}

	updated := &xray.ClientTraffic{}
	if err := db.Model(xray.ClientTraffic{}).Where("id = ?", row.Id).First(updated).Error; err != nil {
		t.Fatalf("reload row: %v", err)
	}
	if !updated.Enable || updated.Total != 5 {
		t.Errorf("case-mismatched email not updated: enable=%v total=%d", updated.Enable, updated.Total)
	}

	if err := svc.DelClientStat(db, "USER@example.com"); err != nil {
		t.Fatalf("DelClientStat: %v", err)
	}
	var count int64
	if err := db.Model(xray.ClientTraffic{}).Where("id = ?", row.Id).Count(&count).Error; err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 0 {
		t.Errorf("case-mismatched email row not deleted")
	}
}

// P6 regression: legacy clients marked with the "[reseller:username]" comment
// tag are recognized as owned by that reseller.
func TestClientOwnedByReseller_LegacyCommentTag(t *testing.T) {
	user := &model.User{Id: 7, Username: "bob"}

	if !ClientOwnedByReseller(model.Client{OwnerUserID: 7}, user) {
		t.Errorf("owner user id match should own the client")
	}
	if !ClientOwnedByReseller(model.Client{Comment: "note [reseller:bob]"}, user) {
		t.Errorf("legacy comment tag should own the client")
	}
	if !ClientOwnedByReseller(model.Client{Comment: "[reseller:bob]"}, user) {
		t.Errorf("comment tag alone should own the client")
	}
	if ClientOwnedByReseller(model.Client{Comment: "[reseller:alice]"}, user) {
		t.Errorf("another reseller's tag must not own the client")
	}
	if ClientOwnedByReseller(model.Client{Email: "x@t"}, user) {
		t.Errorf("unrelated client must not be owned")
	}
	if ClientOwnedByReseller(model.Client{OwnerUserID: 7}, nil) {
		t.Errorf("nil user must not own anything")
	}
}

// P8 regression: inbounds without a user in an imported backup must be
// attributed to the primary admin, never to a lower-id reseller.
func TestImportLegacyBackup_InboundAttributedToPrimaryAdmin(t *testing.T) {
	initLifecycleTestDB(t)
	db := database.GetDB()

	data := &legacyBackupData{
		Users: []legacyUser{
			{Id: 2, Username: "res2", Role: "reseller", ParentUserID: 5, Enabled: true, Password: "$2a$10$abcdefghijklmnopqrstuv"},
			{Id: 5, Username: "admin5", Role: "admin", Enabled: true, Password: "$2a$10$abcdefghijklmnopqrstuv"},
		},
		Inbounds: []legacyInbound{
			{Id: 11, UserId: 0, Port: 42001, Protocol: "vless", Tag: "imported-in", Enable: true, Settings: "{}"},
		},
	}
	if err := importLegacyBackupData(db, data, false, nil); err != nil {
		t.Fatalf("importLegacyBackupData: %v", err)
	}

	inbound := &model.Inbound{}
	if err := db.Model(model.Inbound{}).Where("id = ?", 11).First(inbound).Error; err != nil {
		t.Fatalf("reload imported inbound: %v", err)
	}
	if inbound.UserId != 5 {
		t.Errorf("imported inbound user_id = %d, want primary admin 5 (not reseller 2)", inbound.UserId)
	}
}

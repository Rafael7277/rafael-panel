package service

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/database/model"
)

// Regression matrix for the disable-reason marker (Build 38 hardening).
//
// The client lifecycle distinguishes:
//   - manual disable  (marker "manual"): never reversed by a quota increase,
//     an expiry extension, a usage reset, the periodic traffic job, or the
//     reseller restore.
//   - automatic disable (marker "quota" / "expiry"): reversed when the new
//     limits resolve the condition, subject to the traffic job's cohort check.
//   - reseller lifecycle disable (marker "reseller"): only reversed by the
//     lifecycle restore, and only when the client is eligible.
//   - legacy disable (no marker, pre-marker data): the old-limits heuristic
//     decides, preserving the pre-Build-38 behavior.

// runTrafficTick simulates one run of the periodic traffic job (xray not
// running: the runtime API calls are skipped and the DB stays authoritative).
func runTrafficTick(t *testing.T) {
	t.Helper()
	if err, _ := (&InboundService{}).AddTraffic(nil, nil); err != nil {
		t.Fatalf("traffic tick: %v", err)
	}
}

func assertClientEnabled(t *testing.T, inboundId int, email string) {
	t.Helper()
	c := findClientByEmail(t, reloadInboundClients(t, inboundId), email)
	if !c.Enable {
		t.Errorf("client %s should be enabled in settings", email)
	}
	if c.DisableReason != "" {
		t.Errorf("enabled client %s must carry no disable reason, got %q", email, c.DisableReason)
	}
	if row := reloadTrafficRow(t, email); !row.Enable {
		t.Errorf("client_traffics row for %s should be enabled (settings/stats must agree)", email)
	}
}

func assertClientDisabled(t *testing.T, inboundId int, email, wantReason string) {
	t.Helper()
	c := findClientByEmail(t, reloadInboundClients(t, inboundId), email)
	if c.Enable {
		t.Errorf("client %s should be disabled in settings", email)
	}
	if c.DisableReason != wantReason {
		t.Errorf("client %s disable reason = %q, want %q", email, c.DisableReason, wantReason)
	}
	if row := reloadTrafficRow(t, email); row.Enable {
		t.Errorf("client_traffics row for %s should be disabled (settings/stats must agree)", email)
	}
}

func toggleClient(t *testing.T, email string) bool {
	t.Helper()
	enabled, _, err := (&InboundService{}).ToggleClientEnableByEmail(email)
	if err != nil {
		t.Fatalf("toggle client %s: %v", email, err)
	}
	return enabled
}

// --- Matrix A: client lifecycle (manual vs automatic disable) ---

// A1: a manual disable of a depleted client is not reversed by a quota increase.
func TestDisableMarker_ManualOffWhileDepleted_NotReenabledByQuotaRaise(t *testing.T) {
	initLifecycleTestDB(t)

	client := model.Client{
		ID: "11111111-1111-4111-8111-111111111111", Email: "manual-depleted@t",
		Enable: true, TotalGB: 1000,
	}
	inbound := createInboundWithClients(t, 1, "a1-vless", 41101, model.VLESS, []model.Client{client}, map[string]any{"decryption": "none"})
	setClientUsage(t, "manual-depleted@t", 600, 400, true) // usage == limit

	if toggleClient(t, "manual-depleted@t") {
		t.Fatalf("toggle should have disabled the client")
	}
	assertClientDisabled(t, inbound.Id, "manual-depleted@t", DisableReasonManual)

	// Raise the volume (administrative quota extension): must stay disabled.
	if _, err := (&InboundService{}).ResetClientTrafficLimitByEmail("manual-depleted@t", 100); err != nil {
		t.Fatalf("ResetClientTrafficLimitByEmail: %v", err)
	}
	assertClientDisabled(t, inbound.Id, "manual-depleted@t", DisableReasonManual)

	// And the traffic job must not touch it either.
	for i := 0; i < 3; i++ {
		runTrafficTick(t)
	}
	assertClientDisabled(t, inbound.Id, "manual-depleted@t", DisableReasonManual)
}

// A2: a manual disable of an expired client is not reversed by an expiry extension.
func TestDisableMarker_ManualOffWhileExpired_NotReenabledByExpiryExtension(t *testing.T) {
	initLifecycleTestDB(t)

	past := time.Now().UnixMilli() - 86400000
	client := model.Client{
		ID: "11111111-1111-4111-8111-111111111111", Email: "manual-expired@t",
		Enable: true, ExpiryTime: past,
	}
	inbound := createInboundWithClients(t, 1, "a2-vless", 41102, model.VLESS, []model.Client{client}, map[string]any{"decryption": "none"})
	setClientUsage(t, "manual-expired@t", 0, 0, true)

	if toggleClient(t, "manual-expired@t") {
		t.Fatalf("toggle should have disabled the client")
	}
	assertClientDisabled(t, inbound.Id, "manual-expired@t", DisableReasonManual)

	future := time.Now().UnixMilli() + 30*86400000
	if _, err := (&InboundService{}).ResetClientExpiryTimeByEmail("manual-expired@t", future); err != nil {
		t.Fatalf("ResetClientExpiryTimeByEmail: %v", err)
	}
	assertClientDisabled(t, inbound.Id, "manual-expired@t", DisableReasonManual)
}

// A3: an automatic quota disable is reversed by a quota increase.
func TestDisableMarker_AutoQuotaOff_ReenabledByQuotaRaise(t *testing.T) {
	initLifecycleTestDB(t)

	client := model.Client{
		ID: "11111111-1111-4111-8111-111111111111", Email: "auto-quota@t",
		Enable: true, TotalGB: 1000,
	}
	inbound := createInboundWithClients(t, 1, "a3-vless", 41103, model.VLESS, []model.Client{client}, map[string]any{"decryption": "none"})
	setClientUsage(t, "auto-quota@t", 600, 400, true)

	runTrafficTick(t)
	assertClientDisabled(t, inbound.Id, "auto-quota@t", DisableReasonQuota)

	if _, err := (&InboundService{}).ResetClientTrafficLimitByEmail("auto-quota@t", 100); err != nil {
		t.Fatalf("ResetClientTrafficLimitByEmail: %v", err)
	}
	assertClientEnabled(t, inbound.Id, "auto-quota@t")
}

// A4: an automatic expiry disable is reversed by an expiry extension.
func TestDisableMarker_AutoExpiryOff_ReenabledByExpiryExtension(t *testing.T) {
	initLifecycleTestDB(t)

	past := time.Now().UnixMilli() - 86400000
	client := model.Client{
		ID: "11111111-1111-4111-8111-111111111111", Email: "auto-expired@t",
		Enable: true, ExpiryTime: past,
	}
	inbound := createInboundWithClients(t, 1, "a4-vless", 41104, model.VLESS, []model.Client{client}, map[string]any{"decryption": "none"})
	setClientUsage(t, "auto-expired@t", 0, 0, true)

	runTrafficTick(t)
	assertClientDisabled(t, inbound.Id, "auto-expired@t", DisableReasonExpiry)

	future := time.Now().UnixMilli() + 30*86400000
	if _, err := (&InboundService{}).ResetClientExpiryTimeByEmail("auto-expired@t", future); err != nil {
		t.Fatalf("ResetClientExpiryTimeByEmail: %v", err)
	}
	assertClientEnabled(t, inbound.Id, "auto-expired@t")
}

// A5: a manual disable of a non-depleted client survives a quota raise via a
// modal edit (even when the edit payload drops the marker) and repeated ticks.
func TestDisableMarker_ManualOffNotDepleted_SurvivesEditAndTicks(t *testing.T) {
	initLifecycleTestDB(t)

	client := model.Client{
		ID: "11111111-1111-4111-8111-111111111111", Email: "manual-fine@t",
		Enable: true, TotalGB: 100000,
	}
	inbound := createInboundWithClients(t, 1, "a5-vless", 41105, model.VLESS, []model.Client{client}, map[string]any{"decryption": "none"})
	setClientUsage(t, "manual-fine@t", 100, 100, true)

	if toggleClient(t, "manual-fine@t") {
		t.Fatalf("toggle should have disabled the client")
	}
	assertClientDisabled(t, inbound.Id, "manual-fine@t", DisableReasonManual)

	// Modal edit raising the quota, enable=false round-tripped, marker dropped
	// by the frontend: the stored manual marker must still win.
	payload := client
	payload.TotalGB = 200000
	payload.Enable = false
	payload.DisableReason = ""
	if _, err := (&InboundService{}).UpdateInboundClient(&model.Inbound{
		Id:       inbound.Id,
		Settings: clientsSettings(t, []model.Client{payload}),
	}, payload.ID); err != nil {
		t.Fatalf("UpdateInboundClient: %v", err)
	}
	assertClientDisabled(t, inbound.Id, "manual-fine@t", DisableReasonManual)

	for i := 0; i < 3; i++ {
		runTrafficTick(t)
	}
	assertClientDisabled(t, inbound.Id, "manual-fine@t", DisableReasonManual)
}

// A6/A7 (task 5): an edit of an auto-disabled client whose limits still
// deplete it must leave it disabled, so the next tick agrees with the edit.
func TestDisableMarker_EditedWhileStillDepleted_StaysDisabled(t *testing.T) {
	initLifecycleTestDB(t)

	client := model.Client{
		ID: "11111111-1111-4111-8111-111111111111", Email: "still-depleted@t",
		Enable: true, TotalGB: 1000,
	}
	inbound := createInboundWithClients(t, 1, "a6-vless", 41106, model.VLESS, []model.Client{client}, map[string]any{"decryption": "none"})
	setClientUsage(t, "still-depleted@t", 600, 400, true)

	runTrafficTick(t)
	assertClientDisabled(t, inbound.Id, "still-depleted@t", DisableReasonQuota)

	// Edit keeping the same exhausted limits, enable=false round-tripped.
	payload := client
	payload.Enable = false
	payload.DisableReason = ""
	if _, err := (&InboundService{}).UpdateInboundClient(&model.Inbound{
		Id:       inbound.Id,
		Settings: clientsSettings(t, []model.Client{payload}),
	}, payload.ID); err != nil {
		t.Fatalf("UpdateInboundClient: %v", err)
	}
	assertClientDisabled(t, inbound.Id, "still-depleted@t", DisableReasonQuota)

	runTrafficTick(t)
	assertClientDisabled(t, inbound.Id, "still-depleted@t", DisableReasonQuota)
}

func TestDisableMarker_EditedWhileStillExpired_StaysDisabled(t *testing.T) {
	initLifecycleTestDB(t)

	past := time.Now().UnixMilli() - 86400000
	client := model.Client{
		ID: "11111111-1111-4111-8111-111111111111", Email: "still-expired@t",
		Enable: true, ExpiryTime: past,
	}
	inbound := createInboundWithClients(t, 1, "a7-vless", 41107, model.VLESS, []model.Client{client}, map[string]any{"decryption": "none"})
	setClientUsage(t, "still-expired@t", 0, 0, true)

	runTrafficTick(t)
	assertClientDisabled(t, inbound.Id, "still-expired@t", DisableReasonExpiry)

	payload := client
	payload.Enable = false
	payload.DisableReason = ""
	if _, err := (&InboundService{}).UpdateInboundClient(&model.Inbound{
		Id:       inbound.Id,
		Settings: clientsSettings(t, []model.Client{payload}),
	}, payload.ID); err != nil {
		t.Fatalf("UpdateInboundClient: %v", err)
	}
	assertClientDisabled(t, inbound.Id, "still-expired@t", DisableReasonExpiry)
}

// A8 (task 5, flicker prevention): a quota raise must not re-enable a client
// whose cohort (same subscription id) is still depleted by a sibling — the
// traffic job would disable it again on the very next tick.
func TestDisableMarker_QuotaRaisedButCohortSiblingStillDepleted_StaysDisabled(t *testing.T) {
	initLifecycleTestDB(t)

	a := model.Client{
		ID: "11111111-1111-4111-8111-111111111111", Email: "sibling-a@t",
		Enable: true, TotalGB: 1000, SubID: "shared-sub",
	}
	b := model.Client{
		ID: "22222222-2222-4222-8222-222222222222", Email: "sibling-b@t",
		Enable: true, TotalGB: 100000, SubID: "shared-sub",
	}
	inbound := createInboundWithClients(t, 1, "a8-vless", 41108, model.VLESS, []model.Client{a, b}, map[string]any{"decryption": "none"})
	setClientUsage(t, "sibling-a@t", 0, 0, true)
	setClientUsage(t, "sibling-b@t", 60000, 40000, true) // cohort used == max limit

	runTrafficTick(t)
	assertClientDisabled(t, inbound.Id, "sibling-a@t", DisableReasonQuota)
	assertClientDisabled(t, inbound.Id, "sibling-b@t", DisableReasonQuota)

	// Raise only sibling A's volume: its own limits resolve, but the cohort
	// (100000 used vs sibling B's 100000 limit) is still depleted.
	payload := a
	payload.TotalGB = 100000
	payload.Enable = false
	payload.DisableReason = ""
	if _, err := (&InboundService{}).UpdateInboundClient(&model.Inbound{
		Id:       inbound.Id,
		Settings: clientsSettings(t, []model.Client{payload}),
	}, payload.ID); err != nil {
		t.Fatalf("UpdateInboundClient: %v", err)
	}
	assertClientDisabled(t, inbound.Id, "sibling-a@t", DisableReasonQuota)

	runTrafficTick(t)
	assertClientDisabled(t, inbound.Id, "sibling-a@t", DisableReasonQuota)
	assertClientDisabled(t, inbound.Id, "sibling-b@t", DisableReasonQuota)
}

// A9 (task 5): a quota raise on an auto-disabled client re-enables it AND the
// next traffic tick agrees (no enable/disable flicker).
func TestDisableMarker_QuotaRaisedViaEdit_NoFlickerOnNextTick(t *testing.T) {
	initLifecycleTestDB(t)

	client := model.Client{
		ID: "11111111-1111-4111-8111-111111111111", Email: "flicker@t",
		Enable: true, TotalGB: 1000,
	}
	inbound := createInboundWithClients(t, 1, "a9-vless", 41109, model.VLESS, []model.Client{client}, map[string]any{"decryption": "none"})
	setClientUsage(t, "flicker@t", 600, 400, true)

	runTrafficTick(t)
	assertClientDisabled(t, inbound.Id, "flicker@t", DisableReasonQuota)

	payload := client
	payload.TotalGB = 100000
	payload.Enable = false
	payload.DisableReason = ""
	if _, err := (&InboundService{}).UpdateInboundClient(&model.Inbound{
		Id:       inbound.Id,
		Settings: clientsSettings(t, []model.Client{payload}),
	}, payload.ID); err != nil {
		t.Fatalf("UpdateInboundClient: %v", err)
	}
	assertClientEnabled(t, inbound.Id, "flicker@t")

	for i := 0; i < 3; i++ {
		runTrafficTick(t)
	}
	assertClientEnabled(t, inbound.Id, "flicker@t")
}

// A10: the periodic renewal (autoRenewClients) renews a manually disabled
// periodic client's expiry and usage but never re-enables it.
func TestAutoRenew_DoesNotReenableManuallyDisabledClient(t *testing.T) {
	initLifecycleTestDB(t)

	// Use a multiple-of-reset distance in the past so the renewed expiry lands
	// comfortably in the future, not exactly at "now".
	past := time.Now().UnixMilli() - 10*86400000
	manual := model.Client{
		ID: "11111111-1111-4111-8111-111111111111", Email: "renew-manual@t",
		Enable: false, DisableReason: DisableReasonManual, ExpiryTime: past, Reset: 3,
	}
	legacy := model.Client{
		ID: "22222222-2222-4222-8222-222222222222", Email: "renew-legacy@t",
		Enable: false, ExpiryTime: past, Reset: 3,
	}
	inbound := createInboundWithClients(t, 1, "a10-vless", 41110, model.VLESS, []model.Client{manual, legacy}, map[string]any{"decryption": "none"})
	setClientUsage(t, "renew-manual@t", 500, 500, false)
	setClientUsage(t, "renew-legacy@t", 500, 500, false)

	runTrafficTick(t)

	// Both were renewed (expiry in the future, usage reset) ...
	for _, email := range []string{"renew-manual@t", "renew-legacy@t"} {
		c := findClientByEmail(t, reloadInboundClients(t, inbound.Id), email)
		if c.ExpiryTime <= time.Now().UnixMilli() {
			t.Errorf("%s expiry was not renewed: %d", email, c.ExpiryTime)
		}
		row := reloadTrafficRow(t, email)
		if row.Up != 0 || row.Down != 0 {
			t.Errorf("%s usage was not reset: up=%d down=%d", email, row.Up, row.Down)
		}
	}

	// ... but only the legacy disable is re-enabled; the manual one is not.
	assertClientDisabled(t, inbound.Id, "renew-manual@t", DisableReasonManual)
	assertClientEnabled(t, inbound.Id, "renew-legacy@t")
}

// --- Matrix B: reseller lifecycle restore eligibility ---

// B1: a recorded client still depleted by its own quota is not restored; it is
// restored once the condition resolves.
func TestResellerRestore_SkipsStillDepletedClient_ThenRestoresWhenResolved(t *testing.T) {
	initLifecycleTestDB(t)
	admin, reseller := createAdminAndReseller(t, 100000)

	a := model.Client{
		ID: "11111111-1111-4111-8111-111111111111", Email: "r-depleted@t",
		Enable: true, TotalGB: 1000, OwnerUserID: reseller.Id,
	}
	inbound := createInboundWithClients(t, reseller.Id, "b1-vless", 41111, model.VLESS, []model.Client{a}, map[string]any{"decryption": "none"})
	setClientUsage(t, "r-depleted@t", 600, 400, true)

	us := &UserService{}
	if _, err := us.UpdateResellerWithOptions(admin.Id, reseller.Id, reseller.Username, "", 10, 0, false,
		ResellerPermissions{}, nil, ResellerUpdateOptions{ClientAction: ResellerLifecycleDisable}); err != nil {
		t.Fatalf("disable reseller: %v", err)
	}
	assertClientDisabled(t, inbound.Id, "r-depleted@t", DisableReasonReseller)
	wantKey := resellerClientStateKey(inbound.Id, "r-depleted@t")
	if state := parseManagedState(reloadReseller(t, reseller.Id).ManagedState); len(state.AutoDisabledClientKeys) != 1 {
		t.Fatalf("lifecycle disable must record the client: %#v", state.AutoDisabledClientKeys)
	}

	// Re-enable the reseller: the client is still depleted, so it must NOT be
	// restored, and the key must be kept for a later restore.
	if _, err := us.UpdateResellerWithOptions(admin.Id, reseller.Id, reseller.Username, "", 10, 0, true,
		ResellerPermissions{}, nil, ResellerUpdateOptions{ClientAction: ResellerLifecycleNone}); err != nil {
		t.Fatalf("re-enable reseller: %v", err)
	}
	assertClientDisabled(t, inbound.Id, "r-depleted@t", DisableReasonReseller)
	if state := parseManagedState(reloadReseller(t, reseller.Id).ManagedState); len(state.AutoDisabledClientKeys) != 1 || state.AutoDisabledClientKeys[0] != wantKey {
		t.Fatalf("depleted client key must be kept for a later restore: %#v", state.AutoDisabledClientKeys)
	}

	// Resolve the condition: the next sync restores the client.
	setClientUsage(t, "r-depleted@t", 0, 0, false)
	if err := us.SyncResellerRuntimeState(reseller.Id); err != nil {
		t.Fatalf("sync after usage reset: %v", err)
	}
	assertClientEnabled(t, inbound.Id, "r-depleted@t")
	if state := parseManagedState(reloadReseller(t, reseller.Id).ManagedState); len(state.AutoDisabledClientKeys) != 0 {
		t.Errorf("managed state should be empty after restore: %#v", state.AutoDisabledClientKeys)
	}

	// Repeated syncs are stable.
	for i := 0; i < 2; i++ {
		if err := us.SyncResellerRuntimeState(reseller.Id); err != nil {
			t.Fatalf("repeat sync: %v", err)
		}
	}
	assertClientEnabled(t, inbound.Id, "r-depleted@t")
}

// B2: a recorded client still expired is not restored.
func TestResellerRestore_SkipsStillExpiredClient(t *testing.T) {
	initLifecycleTestDB(t)
	admin, reseller := createAdminAndReseller(t, 100000)

	past := time.Now().UnixMilli() - 86400000
	a := model.Client{
		ID: "11111111-1111-4111-8111-111111111111", Email: "r-expired@t",
		Enable: true, ExpiryTime: past, OwnerUserID: reseller.Id,
	}
	inbound := createInboundWithClients(t, reseller.Id, "b2-vless", 41112, model.VLESS, []model.Client{a}, map[string]any{"decryption": "none"})
	setClientUsage(t, "r-expired@t", 0, 0, true)

	us := &UserService{}
	if _, err := us.UpdateResellerWithOptions(admin.Id, reseller.Id, reseller.Username, "", 10, 0, false,
		ResellerPermissions{}, nil, ResellerUpdateOptions{ClientAction: ResellerLifecycleDisable}); err != nil {
		t.Fatalf("disable reseller: %v", err)
	}

	if _, err := us.UpdateResellerWithOptions(admin.Id, reseller.Id, reseller.Username, "", 10, 0, true,
		ResellerPermissions{}, nil, ResellerUpdateOptions{ClientAction: ResellerLifecycleNone}); err != nil {
		t.Fatalf("re-enable reseller: %v", err)
	}
	assertClientDisabled(t, inbound.Id, "r-expired@t", DisableReasonReseller)
	wantKey := resellerClientStateKey(inbound.Id, "r-expired@t")
	if state := parseManagedState(reloadReseller(t, reseller.Id).ManagedState); len(state.AutoDisabledClientKeys) != 1 || state.AutoDisabledClientKeys[0] != wantKey {
		t.Fatalf("expired client key must be kept (not eligible yet): %#v", state.AutoDisabledClientKeys)
	}
}

// B3: a client manually disabled after the lifecycle disable is never
// force-enabled by the restore, and the recorded key is dropped.
func TestResellerRestore_ManuallyDisabledAfterLifecycle_NotForceEnabled(t *testing.T) {
	initLifecycleTestDB(t)
	admin, reseller := createAdminAndReseller(t, 100000)

	a := model.Client{
		ID: "11111111-1111-4111-8111-111111111111", Email: "r-manual@t",
		Enable: true, OwnerUserID: reseller.Id,
	}
	inbound := createInboundWithClients(t, reseller.Id, "b3-vless", 41113, model.VLESS, []model.Client{a}, map[string]any{"decryption": "none"})
	setClientUsage(t, "r-manual@t", 0, 0, true)

	us := &UserService{}
	if _, err := us.UpdateResellerWithOptions(admin.Id, reseller.Id, reseller.Username, "", 10, 0, false,
		ResellerPermissions{}, nil, ResellerUpdateOptions{ClientAction: ResellerLifecycleDisable}); err != nil {
		t.Fatalf("disable reseller: %v", err)
	}
	assertClientDisabled(t, inbound.Id, "r-manual@t", DisableReasonReseller)

	// An admin re-enables it manually, then disables it manually again.
	if !toggleClient(t, "r-manual@t") {
		t.Fatalf("manual enable failed")
	}
	if toggleClient(t, "r-manual@t") {
		t.Fatalf("manual disable failed")
	}
	assertClientDisabled(t, inbound.Id, "r-manual@t", DisableReasonManual)

	// Re-enable the reseller: the manual disable must win.
	if _, err := us.UpdateResellerWithOptions(admin.Id, reseller.Id, reseller.Username, "", 10, 0, true,
		ResellerPermissions{}, nil, ResellerUpdateOptions{ClientAction: ResellerLifecycleNone}); err != nil {
		t.Fatalf("re-enable reseller: %v", err)
	}
	assertClientDisabled(t, inbound.Id, "r-manual@t", DisableReasonManual)
	if state := parseManagedState(reloadReseller(t, reseller.Id).ManagedState); len(state.AutoDisabledClientKeys) != 0 {
		t.Errorf("manually disabled client key must be dropped: %#v", state.AutoDisabledClientKeys)
	}

	// Repeated syncs stay stable.
	for i := 0; i < 2; i++ {
		if err := us.SyncResellerRuntimeState(reseller.Id); err != nil {
			t.Fatalf("repeat sync: %v", err)
		}
	}
	assertClientDisabled(t, inbound.Id, "r-manual@t", DisableReasonManual)
}

// B4 (task 5): a reseller-lifecycle-disabled client is not re-enabled by a
// client edit (quota raise) — only the lifecycle restore re-enables it.
func TestResellerLifecycleDisabledClient_NotReenabledByEdit(t *testing.T) {
	initLifecycleTestDB(t)
	admin, reseller := createAdminAndReseller(t, 100000)

	a := model.Client{
		ID: "11111111-1111-4111-8111-111111111111", Email: "r-edit@t",
		Enable: true, TotalGB: 1000, OwnerUserID: reseller.Id,
	}
	inbound := createInboundWithClients(t, reseller.Id, "b4-vless", 41114, model.VLESS, []model.Client{a}, map[string]any{"decryption": "none"})
	setClientUsage(t, "r-edit@t", 100, 100, true)

	us := &UserService{}
	if _, err := us.UpdateResellerWithOptions(admin.Id, reseller.Id, reseller.Username, "", 10, 0, false,
		ResellerPermissions{}, nil, ResellerUpdateOptions{ClientAction: ResellerLifecycleDisable}); err != nil {
		t.Fatalf("disable reseller: %v", err)
	}
	assertClientDisabled(t, inbound.Id, "r-edit@t", DisableReasonReseller)

	// Admin edits the client (raise quota, enable=false round-tripped, marker
	// dropped by the frontend): the lifecycle hold must survive the edit.
	payload := a
	payload.TotalGB = 100000
	payload.Enable = false
	payload.DisableReason = ""
	if _, err := (&InboundService{}).UpdateInboundClient(&model.Inbound{
		Id:       inbound.Id,
		Settings: clientsSettings(t, []model.Client{payload}),
	}, payload.ID); err != nil {
		t.Fatalf("UpdateInboundClient: %v", err)
	}
	assertClientDisabled(t, inbound.Id, "r-edit@t", DisableReasonReseller)

	// The lifecycle restore then re-enables it once the reseller is enabled.
	if _, err := us.UpdateResellerWithOptions(admin.Id, reseller.Id, reseller.Username, "", 10, 0, true,
		ResellerPermissions{}, nil, ResellerUpdateOptions{ClientAction: ResellerLifecycleNone}); err != nil {
		t.Fatalf("re-enable reseller: %v", err)
	}
	assertClientEnabled(t, inbound.Id, "r-edit@t")
}

// B5 (task 7): keys of deleted (nonexistent) clients are removed from the
// managed state; restore ignores them without error, and repeated restores
// are stable.
func TestManagedState_StaleKeyRemovedWhenClientDeleted(t *testing.T) {
	initLifecycleTestDB(t)
	db := database.GetDB()
	admin, reseller := createAdminAndReseller(t, 100000)

	a := model.Client{
		ID: "11111111-1111-4111-8111-111111111111", Email: "r-deleted@t",
		Enable: true, OwnerUserID: reseller.Id,
	}
	inbound := createInboundWithClients(t, reseller.Id, "b5-vless", 41115, model.VLESS, []model.Client{a}, map[string]any{"decryption": "none"})
	setClientUsage(t, "r-deleted@t", 0, 0, true)

	us := &UserService{}
	if _, err := us.UpdateResellerWithOptions(admin.Id, reseller.Id, reseller.Username, "", 10, 0, false,
		ResellerPermissions{}, nil, ResellerUpdateOptions{ClientAction: ResellerLifecycleDisable}); err != nil {
		t.Fatalf("disable reseller: %v", err)
	}

	// Delete the client outright (admin action) while the reseller is off.
	if _, err := (&InboundService{}).DelInboundClientByEmail(inbound.Id, "r-deleted@t"); err != nil {
		t.Fatalf("delete client: %v", err)
	}
	// Add one more key pointing at a client that never existed.
	if err := db.Model(model.User{}).Where("id = ?", reseller.Id).Update("managed_state",
		serializeManagedState(resellerManagedState{AutoDisabledClientKeys: []string{
			resellerClientStateKey(inbound.Id, "r-deleted@t"),
			"999|ghost@t",
		}})).Error; err != nil {
		t.Fatalf("seed stale keys: %v", err)
	}

	// Re-enable the reseller: both stale keys must be dropped, no error.
	if _, err := us.UpdateResellerWithOptions(admin.Id, reseller.Id, reseller.Username, "", 10, 0, true,
		ResellerPermissions{}, nil, ResellerUpdateOptions{ClientAction: ResellerLifecycleNone}); err != nil {
		t.Fatalf("re-enable reseller: %v", err)
	}
	if state := parseManagedState(reloadReseller(t, reseller.Id).ManagedState); len(state.AutoDisabledClientKeys) != 0 {
		t.Errorf("stale keys must be removed: %#v", state.AutoDisabledClientKeys)
	}

	for i := 0; i < 2; i++ {
		if err := us.SyncResellerRuntimeState(reseller.Id); err != nil {
			t.Fatalf("repeat sync: %v", err)
		}
	}
	if state := parseManagedState(reloadReseller(t, reseller.Id).ManagedState); len(state.AutoDisabledClientKeys) != 0 {
		t.Errorf("repeated syncs must stay clean: %#v", state.AutoDisabledClientKeys)
	}
}

// --- Matrix C: reset operations ---

// C1: a usage reset on a manually disabled client resets the usage but never
// re-enables it; settings and client_traffics stay consistent.
func TestResetClientTraffic_ManuallyDisabled_StaysDisabled(t *testing.T) {
	initLifecycleTestDB(t)

	client := model.Client{
		ID: "11111111-1111-4111-8111-111111111111", Email: "reset-manual@t",
		Enable: true, TotalGB: 1000,
	}
	inbound := createInboundWithClients(t, 1, "c1-vless", 41116, model.VLESS, []model.Client{client}, map[string]any{"decryption": "none"})
	setClientUsage(t, "reset-manual@t", 300, 200, true)

	if toggleClient(t, "reset-manual@t") {
		t.Fatalf("toggle should have disabled the client")
	}
	setClientUsage(t, "reset-manual@t", 300, 200, false)

	if _, err := (&InboundService{}).ResetClientTrafficByEmail("reset-manual@t"); err != nil {
		t.Fatalf("ResetClientTrafficByEmail: %v", err)
	}
	assertClientDisabled(t, inbound.Id, "reset-manual@t", DisableReasonManual)
	if row := reloadTrafficRow(t, "reset-manual@t"); row.Up != 0 || row.Down != 0 {
		t.Errorf("usage should be reset: up=%d down=%d", row.Up, row.Down)
	}
}

// C2: a usage reset on an automatically quota-disabled client re-enables it
// (usage reset resolves the depletion), with usage zeroed and marker cleared.
func TestResetClientTraffic_AutoQuotaDisabled_Reenabled(t *testing.T) {
	initLifecycleTestDB(t)

	client := model.Client{
		ID: "11111111-1111-4111-8111-111111111111", Email: "reset-auto@t",
		Enable: true, TotalGB: 1000,
	}
	inbound := createInboundWithClients(t, 1, "c2-vless", 41117, model.VLESS, []model.Client{client}, map[string]any{"decryption": "none"})
	setClientUsage(t, "reset-auto@t", 600, 400, true)

	runTrafficTick(t)
	assertClientDisabled(t, inbound.Id, "reset-auto@t", DisableReasonQuota)

	if _, err := (&InboundService{}).ResetClientTrafficByEmail("reset-auto@t"); err != nil {
		t.Fatalf("ResetClientTrafficByEmail: %v", err)
	}
	assertClientEnabled(t, inbound.Id, "reset-auto@t")
	if row := reloadTrafficRow(t, "reset-auto@t"); row.Up != 0 || row.Down != 0 {
		t.Errorf("usage should be reset: up=%d down=%d", row.Up, row.Down)
	}
}

// C3: a usage reset on an expired client resets the usage but keeps it
// disabled (the expiry still holds it).
func TestResetClientTraffic_ExpiredClient_StaysDisabled(t *testing.T) {
	initLifecycleTestDB(t)

	past := time.Now().UnixMilli() - 86400000
	client := model.Client{
		ID: "11111111-1111-4111-8111-111111111111", Email: "reset-expired@t",
		Enable: true, ExpiryTime: past, TotalGB: 1000,
	}
	inbound := createInboundWithClients(t, 1, "c3-vless", 41118, model.VLESS, []model.Client{client}, map[string]any{"decryption": "none"})
	setClientUsage(t, "reset-expired@t", 600, 400, true)

	runTrafficTick(t)
	assertClientDisabled(t, inbound.Id, "reset-expired@t", DisableReasonExpiry)

	if _, err := (&InboundService{}).ResetClientTrafficByEmail("reset-expired@t"); err != nil {
		t.Fatalf("ResetClientTrafficByEmail: %v", err)
	}
	assertClientDisabled(t, inbound.Id, "reset-expired@t", DisableReasonExpiry)
	if row := reloadTrafficRow(t, "reset-expired@t"); row.Up != 0 || row.Down != 0 {
		t.Errorf("usage should be reset: up=%d down=%d", row.Up, row.Down)
	}
}

// C4: repeated resets on an enabled client are stable and keep it enabled.
func TestResetClientTraffic_RepeatedResetStable(t *testing.T) {
	initLifecycleTestDB(t)

	client := model.Client{
		ID: "11111111-1111-4111-8111-111111111111", Email: "reset-repeat@t",
		Enable: true, TotalGB: 100000,
	}
	inbound := createInboundWithClients(t, 1, "c4-vless", 41119, model.VLESS, []model.Client{client}, map[string]any{"decryption": "none"})
	setClientUsage(t, "reset-repeat@t", 100, 100, true)

	for i := 0; i < 2; i++ {
		if _, err := (&InboundService{}).ResetClientTrafficByEmail("reset-repeat@t"); err != nil {
			t.Fatalf("reset %d: %v", i, err)
		}
	}
	assertClientEnabled(t, inbound.Id, "reset-repeat@t")
	if row := reloadTrafficRow(t, "reset-repeat@t"); row.Up != 0 || row.Down != 0 {
		t.Errorf("usage should be reset: up=%d down=%d", row.Up, row.Down)
	}
}

// C5: the per-inbound reset entry point shares the same semantics with the
// by-email one (a manual disable survives a reset).
func TestResetClientTraffic_ByInboundId_ManualDisableSurvives(t *testing.T) {
	initLifecycleTestDB(t)

	client := model.Client{
		ID: "11111111-1111-4111-8111-111111111111", Email: "reset-inbound@t",
		Enable: true, TotalGB: 1000,
	}
	inbound := createInboundWithClients(t, 1, "c5-vless", 41120, model.VLESS, []model.Client{client}, map[string]any{"decryption": "none"})
	setClientUsage(t, "reset-inbound@t", 300, 200, true)

	if toggleClient(t, "reset-inbound@t") {
		t.Fatalf("toggle should have disabled the client")
	}
	setClientUsage(t, "reset-inbound@t", 300, 200, false)

	if _, err := (&InboundService{}).ResetClientTraffic(inbound.Id, "reset-inbound@t"); err != nil {
		t.Fatalf("ResetClientTraffic: %v", err)
	}
	assertClientDisabled(t, inbound.Id, "reset-inbound@t", DisableReasonManual)
	if row := reloadTrafficRow(t, "reset-inbound@t"); row.Up != 0 || row.Down != 0 {
		t.Errorf("usage should be reset: up=%d down=%d", row.Up, row.Down)
	}
}

// --- Matrix E: settings preservation through client edits ---

// E1: editing a client must preserve all protocol-level settings keys
// (shadowsocks method/password/network/ivCheck, vless decryption/fallbacks,
// vmess disableInsecureEncryption) and unknown top-level keys, and must be
// idempotent across repeated edits.
func TestUpdateInboundClient_PreservesProtocolSettings(t *testing.T) {
	initLifecycleTestDB(t)

	ssClient := model.Client{Email: "ss-edit@t", Password: "ss-pass-1", Enable: true}
	ssInbound := createInboundWithClients(t, 1, "e1-ss", 41121, model.Shadowsocks, []model.Client{ssClient}, map[string]any{
		"method": "2022-blake3-aes-128-gcm", "password": "server-pw", "network": "tcp,udp",
		"ivCheck": true, "customTopLevelKey": "keep-me",
	})
	vlessClient := model.Client{ID: "11111111-1111-4111-8111-111111111111", Email: "vless-edit@t", Enable: true}
	vlessInbound := createInboundWithClients(t, 1, "e1-vless", 41122, model.VLESS, []model.Client{vlessClient}, map[string]any{
		"decryption": "none", "fallbacks": []any{map[string]any{"dest": 80, "xver": 0}},
	})
	vmessClient := model.Client{ID: "22222222-2222-4222-8222-222222222222", Email: "vmess-edit@t", Enable: true}
	vmessInbound := createInboundWithClients(t, 1, "e1-vmess", 41123, model.VMESS, []model.Client{vmessClient}, map[string]any{
		"disableInsecureEncryption": true,
	})

	svc := &InboundService{}
	editClient := func(inboundId int, protocol model.Protocol, client model.Client) {
		t.Helper()
		clientId := client.ID
		if protocol == model.Shadowsocks {
			clientId = client.Email
		}
		client.Enable = false // round-tripped disabled: also exercises the marker logic
		if _, err := svc.UpdateInboundClient(&model.Inbound{
			Id:       inboundId,
			Settings: clientsSettings(t, []model.Client{client}),
		}, clientId); err != nil {
			t.Fatalf("edit %s client: %v", protocol, err)
		}
	}

	// Two edit rounds per protocol to prove idempotency.
	for round := 0; round < 2; round++ {
		editClient(ssInbound.Id, model.Shadowsocks, ssClient)
		editClient(vlessInbound.Id, model.VLESS, vlessClient)
		editClient(vmessInbound.Id, model.VMESS, vmessClient)

		assertSettingsKeys := func(inboundId int, want map[string]any) {
			t.Helper()
			inbound, err := svc.GetInbound(inboundId)
			if err != nil {
				t.Fatalf("GetInbound: %v", err)
			}
			var saved map[string]any
			if err := json.Unmarshal([]byte(inbound.Settings), &saved); err != nil {
				t.Fatalf("unmarshal settings: %v", err)
			}
			for key, value := range want {
				switch wantVal := value.(type) {
				case bool:
					if v, ok := saved[key].(bool); !ok || v != wantVal {
						t.Errorf("round %d: settings key %s lost or changed: %#v", round, key, saved[key])
					}
				case string:
					if saved[key] != wantVal {
						t.Errorf("round %d: settings key %s lost or changed: %#v", round, key, saved[key])
					}
				}
			}
		}
		assertSettingsKeys(ssInbound.Id, map[string]any{
			"method": "2022-blake3-aes-128-gcm", "password": "server-pw", "network": "tcp,udp",
			"ivCheck": true, "customTopLevelKey": "keep-me",
		})
		assertSettingsKeys(vlessInbound.Id, map[string]any{"decryption": "none"})
		assertSettingsKeys(vmessInbound.Id, map[string]any{"disableInsecureEncryption": true})

		// The vless fallbacks array must survive intact (not emptied/duplicated).
		vlessIn, err := svc.GetInbound(vlessInbound.Id)
		if err != nil {
			t.Fatalf("GetInbound vless: %v", err)
		}
		var vlessSaved map[string]any
		if err := json.Unmarshal([]byte(vlessIn.Settings), &vlessSaved); err != nil {
			t.Fatalf("unmarshal vless settings: %v", err)
		}
		fallbacks, ok := vlessSaved["fallbacks"].([]any)
		if !ok || len(fallbacks) != 1 {
			t.Errorf("round %d: vless fallbacks lost or corrupted: %#v", round, vlessSaved["fallbacks"])
		}
	}

	// The edited clients were saved disabled from an enabled state (a manual
	// act): they stay disabled with the manual marker, settings and stats in
	// agreement.
	assertClientDisabled(t, ssInbound.Id, "ss-edit@t", DisableReasonManual)
	assertClientDisabled(t, vlessInbound.Id, "vless-edit@t", DisableReasonManual)
	assertClientDisabled(t, vmessInbound.Id, "vmess-edit@t", DisableReasonManual)
}

// --- Matrix F: ownership ---

// F1: username whitespace is trimmed consistently between the ownership rule
// and the legacy comment tag; unrelated resellers and clients never match.
func TestClientOwnedByReseller_UsernameTrimming(t *testing.T) {
	padded := &model.User{Id: 7, Username: "  bob  "}

	if tag := ResellerCommentTag(padded.Username); tag != "[reseller:bob]" {
		t.Errorf("ResellerCommentTag = %q, want [reseller:bob]", tag)
	}
	if !ClientOwnedByReseller(model.Client{Comment: "[reseller:bob]"}, padded) {
		t.Errorf("padded username must own the trimmed tag")
	}
	if !ClientOwnedByReseller(model.Client{Comment: "  note [reseller:bob]  "}, padded) {
		t.Errorf("whitespace around the comment must not break the match")
	}
	if !ClientOwnedByReseller(model.Client{OwnerUserID: 7}, padded) {
		t.Errorf("owner user id must own the client")
	}
	if ClientOwnedByReseller(model.Client{Comment: "[reseller:bob]"}, &model.User{Id: 8, Username: "alice"}) {
		t.Errorf("another reseller must not own the client")
	}
	if ClientOwnedByReseller(model.Client{Email: "x@t", Comment: "no tag"}, padded) {
		t.Errorf("unrelated client must not be owned")
	}

	// The comment helpers round-trip the trimmed tag.
	upserted := UpsertResellerComment("note", "  bob  ")
	if upserted != "note [reseller:bob]" {
		t.Errorf("UpsertResellerComment = %q", upserted)
	}
	if stripped := StripResellerComment(upserted); stripped != "note" {
		t.Errorf("StripResellerComment = %q", stripped)
	}
}

// --- Toggle semantics (task 4) ---

// T1: the manual toggle records/clears the marker and keeps settings and
// client_traffics in sync in both directions.
func TestToggleClientEnable_MarkerAndStatsConsistency(t *testing.T) {
	initLifecycleTestDB(t)

	client := model.Client{
		ID: "11111111-1111-4111-8111-111111111111", Email: "toggle@t",
		Enable: true, TotalGB: 100000,
	}
	inbound := createInboundWithClients(t, 1, "t1-vless", 41124, model.VLESS, []model.Client{client}, map[string]any{"decryption": "none"})
	setClientUsage(t, "toggle@t", 0, 0, true)

	if toggleClient(t, "toggle@t") {
		t.Fatalf("first toggle should disable")
	}
	assertClientDisabled(t, inbound.Id, "toggle@t", DisableReasonManual)

	if !toggleClient(t, "toggle@t") {
		t.Fatalf("second toggle should enable")
	}
	assertClientEnabled(t, inbound.Id, "toggle@t")

	// A manually re-enabled auto-disabled client keeps the plain manual-ON
	// semantics (no invented restrictions).
	setClientUsage(t, "toggle@t", 60000, 40000, true)
	runTrafficTick(t)
	assertClientDisabled(t, inbound.Id, "toggle@t", DisableReasonQuota)
	if !toggleClient(t, "toggle@t") {
		t.Fatalf("toggle on a quota-disabled client should enable it")
	}
	assertClientEnabled(t, inbound.Id, "toggle@t")
}

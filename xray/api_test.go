package xray

import "testing"

// Regression (Build 38 hardening): every XrayAPI method must return a
// meaningful error instead of panicking when called on a zero-value client
// (no running Xray process, Init never called or failed).
func TestXrayAPI_ZeroValue_CallsReturnErrorNotPanic(t *testing.T) {
	api := &XrayAPI{}

	if err := api.AddInbound([]byte(`{"tag":"in","port":1,"protocol":"vless"}`)); err == nil {
		t.Errorf("AddInbound on a zero-value API must return an error, got nil")
	}
	if err := api.DelInbound("in"); err == nil {
		t.Errorf("DelInbound on a zero-value API must return an error, got nil")
	}
	if err := api.AddUser("vless", "in", map[string]any{"email": "a@t", "id": "x"}); err == nil {
		t.Errorf("AddUser on a zero-value API must return an error, got nil")
	}
	if err := api.RemoveUser("in", "a@t"); err == nil {
		t.Errorf("RemoveUser on a zero-value API must return an error, got nil")
	}
	if _, _, err := api.GetTraffic(false); err == nil {
		t.Errorf("GetTraffic on a zero-value API must return an error, got nil")
	}

	// Init with an invalid port must fail without leaving a half-initialized
	// client behind, and the guards above must still hold afterwards.
	if err := api.Init(0); err == nil {
		t.Errorf("Init with port 0 must return an error")
	}
	if err := api.AddUser("vless", "in", map[string]any{"email": "a@t", "id": "x"}); err == nil {
		t.Errorf("AddUser after a failed Init must still return an error")
	}
}

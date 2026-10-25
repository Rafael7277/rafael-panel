package database

import (
	"encoding/json"

	"github.com/mhsanaei/3x-ui/v2/database/model"
	"path/filepath"
	"testing"
)

// getTemplateValue reads the stored xrayTemplateConfig ("" when absent).
func getTemplateValue(t *testing.T) string {
	t.Helper()
	var template model.Setting
	err := GetDB().Where("key = ?", "xrayTemplateConfig").First(&template).Error
	if IsNotFound(err) {
		return ""
	}
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	return template.Value
}

// setTemplateValue writes the xrayTemplateConfig setting.
func setTemplateValue(t *testing.T, value string) {
	t.Helper()
	if err := GetDB().Where("key = ?", "xrayTemplateConfig").Delete(&model.Setting{}).Error; err != nil {
		t.Fatalf("clear template: %v", err)
	}
	if value == "" {
		return
	}
	if err := GetDB().Create(&model.Setting{Key: "xrayTemplateConfig", Value: value}).Error; err != nil {
		t.Fatalf("write template: %v", err)
	}
}

// servicesOf extracts api.services from a template JSON string.
func servicesOf(t *testing.T, value string) []string {
	t.Helper()
	var cfg struct {
		API struct {
			Services []string `json:"services"`
		} `json:"api"`
	}
	if err := json.Unmarshal([]byte(value), &cfg); err != nil {
		t.Fatalf("unmarshal patched template: %v", err)
	}
	return cfg.API.Services
}

func TestEnsureRoutingServiceInTemplate(t *testing.T) {
	if err := InitDB(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer func() { _ = CloseDB(); db = nil }()

	// 1. Legacy template without RoutingService: the service is appended and
	// every other field (including a custom top-level key) survives.
	legacy := `{"log":{"loglevel":"warning"},"api":{"tag":"api","services":["HandlerService","LoggerService","statsService"]},"outbounds":[{"tag":"blocked","protocol":"blackhole"}],"customKey":42}`
	setTemplateValue(t, legacy)
	if err := ensureRoutingServiceInTemplate(); err != nil {
		t.Fatalf("migrate legacy template: %v", err)
	}
	patched := getTemplateValue(t)
	if got := servicesOf(t, patched); len(got) != 4 || got[3] != "RoutingService" || got[0] != "HandlerService" {
		t.Fatalf("services after migration = %v, want original three + appended RoutingService", got)
	}
	var full map[string]any
	if err := json.Unmarshal([]byte(patched), &full); err != nil {
		t.Fatalf("patched template is not valid JSON: %v", err)
	}
	if full["customKey"] != float64(42) {
		t.Fatalf("customKey = %v, want 42 (unrelated keys must survive)", full["customKey"])
	}

	// Idempotent: a second run changes nothing.
	before := getTemplateValue(t)
	if err := ensureRoutingServiceInTemplate(); err != nil {
		t.Fatalf("second migration: %v", err)
	}
	if getTemplateValue(t) != before {
		t.Fatal("second migration must be a no-op")
	}

	// 2. Template without an api block: left untouched.
	noApi := `{"log":{"loglevel":"warning"},"outbounds":[{"tag":"direct","protocol":"freedom"}]}`
	setTemplateValue(t, noApi)
	if err := ensureRoutingServiceInTemplate(); err != nil {
		t.Fatalf("migrate no-api template: %v", err)
	}
	if got := getTemplateValue(t); got != noApi {
		t.Fatalf("no-api template was modified:\n%s", got)
	}

	// 3. Malformed JSON: reported and skipped, not rewritten, no error.
	setTemplateValue(t, `{not json`)
	if err := ensureRoutingServiceInTemplate(); err != nil {
		t.Fatalf("malformed template must not fail: %v", err)
	}
	if got := getTemplateValue(t); got != `{not json` {
		t.Fatalf("malformed template was rewritten:\n%s", got)
	}

	// 4. No template row at all (fresh install path): no-op, no error.
	setTemplateValue(t, "")
	if err := ensureRoutingServiceInTemplate(); err != nil {
		t.Fatalf("missing template must not fail: %v", err)
	}
}

package service

import (
	"encoding/json"
	"fmt"
	"strings"
)

// normalizeVLESSDecryption fills the Xray-required VLESS decryption field when
// it is absent, null, or blank. Other protocols and valid VLESS values are
// preserved exactly as submitted.
func normalizeVLESSDecryption(protocol, rawSettings string) (string, bool, error) {
	if !strings.EqualFold(strings.TrimSpace(protocol), "vless") {
		return rawSettings, false, nil
	}

	settings := make(map[string]any)
	if err := json.Unmarshal([]byte(rawSettings), &settings); err != nil {
		return rawSettings, false, fmt.Errorf("invalid VLESS settings JSON: %w", err)
	}
	if settings == nil {
		return rawSettings, false, fmt.Errorf("VLESS settings must be a JSON object")
	}

	decryption, exists := settings["decryption"]
	if exists && decryption != nil {
		value, ok := decryption.(string)
		if !ok {
			return rawSettings, false, fmt.Errorf("VLESS settings decryption must be a string")
		}
		if strings.TrimSpace(value) != "" {
			return rawSettings, false, nil
		}
	}

	settings["decryption"] = "none"
	normalized, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return rawSettings, false, fmt.Errorf("marshal normalized VLESS settings: %w", err)
	}
	return string(normalized), true, nil
}

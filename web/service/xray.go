package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/logger"
	"github.com/mhsanaei/3x-ui/v2/xray"

	"go.uber.org/atomic"
)

var (
	p                 *xray.Process
	lock              sync.Mutex
	isNeedXrayRestart atomic.Bool // Indicates that restart was requested for Xray
	isManuallyStopped atomic.Bool // Indicates that Xray was stopped manually from the panel
	result            string
)

// XrayService provides business logic for Xray process management.
// It handles starting, stopping, restarting Xray, and managing its configuration.
type XrayService struct {
	inboundService InboundService
	settingService SettingService
	xrayAPI        xray.XrayAPI
}

// IsXrayRunning checks if the Xray process is currently running.
func (s *XrayService) IsXrayRunning() bool {
	return p != nil && p.IsRunning()
}

// GetXrayErr returns the error from the Xray process, if any.
func (s *XrayService) GetXrayErr() error {
	if p == nil {
		return nil
	}

	err := p.GetErr()
	if err == nil {
		return nil
	}

	if runtime.GOOS == "windows" && err.Error() == "exit status 1" {
		// exit status 1 on Windows means that Xray process was killed
		// as we kill process to stop in on Windows, this is not an error
		return nil
	}

	return err
}

// GetXrayResult returns the result string from the Xray process.
func (s *XrayService) GetXrayResult() string {
	if result != "" {
		return result
	}
	if s.IsXrayRunning() {
		return ""
	}
	if p == nil {
		return ""
	}

	result = p.GetResult()

	if runtime.GOOS == "windows" && result == "exit status 1" {
		// exit status 1 on Windows means that Xray process was killed
		// as we kill process to stop in on Windows, this is not an error
		return ""
	}

	return result
}

// GetXrayVersion returns the version of the running Xray process.
func (s *XrayService) GetXrayVersion() string {
	if p == nil {
		return "Unknown"
	}
	return p.GetVersion()
}

// RemoveIndex removes an element at the specified index from a slice.
// Returns a new slice with the element removed.
func RemoveIndex(s []any, index int) []any {
	return append(s[:index], s[index+1:]...)
}

// GetXrayConfig retrieves and builds the Xray configuration from settings and inbounds.
func (s *XrayService) GetXrayConfig() (*xray.Config, error) {
	phaseStart := time.Now()
	logger.Info("Xray startup: loading configuration template")
	templateConfig, err := s.settingService.GetXrayConfigTemplate()
	if err != nil {
		return nil, err
	}
	logger.Infof("Xray startup: configuration template loaded in %s", time.Since(phaseStart))

	xrayConfig := &xray.Config{}
	err = json.Unmarshal([]byte(templateConfig), xrayConfig)
	if err != nil {
		return nil, err
	}
	if len(xrayConfig.RouterConfig) > 0 {
		runtimeRouting, err := adaptiveEngine.ApplyRuntimeRouting(xrayConfig.RouterConfig)
		if err != nil {
			logger.Debug("adaptive smart runtime routing skipped:", err)
		} else {
			xrayConfig.RouterConfig = runtimeRouting
		}
	}

	phaseStart = time.Now()
	logger.Info("Xray startup: reconciling traffic and client state")
	if trafficErr, _ := s.inboundService.AddTraffic(nil, nil); trafficErr != nil {
		logger.Warning("Xray startup: traffic reconciliation failed:", trafficErr)
	}
	logger.Infof("Xray startup: traffic and client state reconciled in %s", time.Since(phaseStart))

	phaseStart = time.Now()
	logger.Info("Xray startup: loading inbounds")
	inbounds, err := s.inboundService.GetAllInbounds()
	if err != nil {
		return nil, err
	}
	logger.Infof("Xray startup: loaded %d inbounds in %s", len(inbounds), time.Since(phaseStart))
	for _, inbound := range inbounds {
		if !inbound.Enable {
			continue
		}
		normalizedSettings, changed, err := normalizeVLESSDecryption(string(inbound.Protocol), inbound.Settings)
		if err != nil {
			return nil, fmt.Errorf("inbound %d (%s): %w", inbound.Id, inbound.Tag, err)
		}
		if changed {
			inbound.Settings = normalizedSettings
			if err := database.GetDB().Model(&model.Inbound{}).Where("id = ?", inbound.Id).Update("settings", normalizedSettings).Error; err != nil {
				logger.Warningf("Unable to persist repaired VLESS decryption for inbound %d (%s): %v", inbound.Id, inbound.Tag, err)
			}
		}
		// get settings clients
		settings := map[string]any{}
		if err := json.Unmarshal([]byte(inbound.Settings), &settings); err != nil {
			return nil, fmt.Errorf("inbound %d (%s) settings JSON: %w", inbound.Id, inbound.Tag, err)
		}
		clients, ok := settings["clients"].([]any)
		if ok {
			// Fast O(N) lookup map for client traffic enablement
			clientStats := inbound.ClientStats
			enableMap := make(map[string]bool, len(clientStats))
			for _, clientTraffic := range clientStats {
				enableMap[clientTraffic.Email] = clientTraffic.Enable
			}

			// filter and clean clients
			var final_clients []any
			for _, client := range clients {
				c, ok := client.(map[string]any)
				if !ok {
					continue
				}

				email, _ := c["email"].(string)

				// check users active or not via stats
				if enable, exists := enableMap[email]; exists && !enable {
					logger.Infof("Remove Inbound User %s due to expiration or traffic limit", email)
					continue
				}

				// check manual disabled flag
				if manualEnable, ok := c["enable"].(bool); ok && !manualEnable {
					continue
				}

				// clear client config for additional parameters
				for key := range c {
					if key != "email" && key != "id" && key != "password" && key != "flow" && key != "method" && key != "auth" {
						delete(c, key)
					}
					if flow, ok := c["flow"].(string); ok && flow == "xtls-rprx-vision-udp443" {
						c["flow"] = "xtls-rprx-vision"
					}
				}
				final_clients = append(final_clients, any(c))
			}

			settings["clients"] = final_clients
			modifiedSettings, err := json.MarshalIndent(settings, "", "  ")
			if err != nil {
				return nil, err
			}

			inbound.Settings = string(modifiedSettings)
		}

		if len(inbound.StreamSettings) > 0 {
			// Unmarshal stream JSON
			var stream map[string]any
			json.Unmarshal([]byte(inbound.StreamSettings), &stream)

			// Remove the "settings" field under "tlsSettings" and "realitySettings"
			tlsSettings, ok1 := stream["tlsSettings"].(map[string]any)
			realitySettings, ok2 := stream["realitySettings"].(map[string]any)
			if ok1 || ok2 {
				if ok1 {
					delete(tlsSettings, "settings")
				} else if ok2 {
					delete(realitySettings, "settings")
				}
			}

			delete(stream, "externalProxy")

			newStream, err := json.MarshalIndent(stream, "", "  ")
			if err != nil {
				return nil, err
			}
			inbound.StreamSettings = string(newStream)
		}

		inboundConfig := inbound.GenXrayInboundConfig()
		xrayConfig.InboundConfigs = append(xrayConfig.InboundConfigs, *inboundConfig)
	}
	return xrayConfig, nil
}

// GetXrayTraffic fetches the current traffic statistics from the running Xray process.
func (s *XrayService) GetXrayTraffic() ([]*xray.Traffic, []*xray.ClientTraffic, error) {
	if !s.IsXrayRunning() {
		err := errors.New("xray is not running")
		logger.Debug("Attempted to fetch Xray traffic, but Xray is not running:", err)
		return nil, nil, err
	}
	apiPort := p.GetAPIPort()
	s.xrayAPI.Init(apiPort)
	defer s.xrayAPI.Close()

	traffic, clientTraffic, err := s.xrayAPI.GetTraffic(true)
	if err != nil {
		logger.Debug("Failed to fetch Xray traffic:", err)
		return nil, nil, err
	}
	return traffic, clientTraffic, nil
}

// RestartXray restarts the Xray process, optionally forcing a restart even if config unchanged.
func (s *XrayService) RestartXray(isForce bool) error {
	phaseStart := time.Now()
	logger.Info("Xray startup: refreshing IP-limit policy")
	if err := clientLimitGate.RefreshFromDB(); err != nil {
		policyErr := fmt.Errorf("prepare IP limit policy before Xray start: %w", err)
		failClosedXrayForIPLimit(policyErr)
		return policyErr
	}
	logger.Infof("Xray startup: IP-limit policy refreshed in %s", time.Since(phaseStart))
	lock.Lock()
	defer lock.Unlock()
	logger.Debug("restart Xray, force:", isForce)
	isManuallyStopped.Store(false)

	phaseStart = time.Now()
	logger.Info("Xray startup: building Xray configuration")
	xrayConfig, err := s.GetXrayConfig()
	if err != nil {
		return err
	}
	logger.Infof("Xray startup: Xray configuration built in %s", time.Since(phaseStart))

	if s.IsXrayRunning() {
		if !isForce && p.GetConfig().Equals(xrayConfig) && !isNeedXrayRestart.Load() {
			logger.Debug("It does not need to restart Xray")
			return nil
		}
		p.Stop()
	}

	p = xray.NewProcess(xrayConfig)
	result = ""
	phaseStart = time.Now()
	logger.Info("Xray startup: launching Xray core")
	err = p.Start()
	if err != nil {
		return err
	}
	logger.Infof("Xray startup: Xray core launch returned in %s", time.Since(phaseStart))

	return nil
}

// failClosedXrayForIPLimit stops serving when the DB policy cannot be
// published. Keeping Xray alive with a stale, more permissive snapshot could
// admit a second source IP after a limit reduction. The regular restart job
// retries once the DB or filesystem problem clears. A user-requested manual
// stop remains authoritative.
func failClosedXrayForIPLimit(policyErr error) {
	lock.Lock()
	defer lock.Unlock()
	if isManuallyStopped.Load() {
		logger.Warning("IP limit policy refresh failed while Xray was manually stopped:", policyErr)
		return
	}
	isNeedXrayRestart.Store(true)
	logger.Error("IP limit policy refresh failed; stopping Xray to avoid stale admission policy:", policyErr)
	if p != nil && p.IsRunning() {
		if err := p.Stop(); err != nil {
			logger.Error("failed to stop Xray after IP limit policy refresh failure:", err)
		}
	}
}

// StopXray stops the running Xray process.
func (s *XrayService) StopXray() error {
	lock.Lock()
	defer lock.Unlock()
	isManuallyStopped.Store(true)
	logger.Debug("Attempting to stop Xray...")
	if s.IsXrayRunning() {
		return p.Stop()
	}
	return errors.New("xray is not running")
}

// SetToNeedRestart marks that Xray needs to be restarted.
func (s *XrayService) SetToNeedRestart() {
	isNeedXrayRestart.Store(true)
}

// IsNeedRestartAndSetFalse checks if restart is needed and resets the flag to false.
func (s *XrayService) IsNeedRestartAndSetFalse() bool {
	return isNeedXrayRestart.CompareAndSwap(true, false)
}

// DidXrayCrash checks if Xray crashed by verifying it's not running and wasn't manually stopped.
func (s *XrayService) DidXrayCrash() bool {
	return !s.IsXrayRunning() && !isManuallyStopped.Load()
}

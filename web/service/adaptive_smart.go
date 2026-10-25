package service

import (
	"context"
	"encoding/json"
	"hash/crc32"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mhsanaei/3x-ui/v2/logger"
)

const (
	adaptiveSmartFallbackStrategy = "leastLoad"
	adaptiveSmartMaxConcurrent    = 1
	adaptiveSmartRuntimeCooldown  = 45 * time.Second
	adaptiveSmartEventHistorySize = 50
)

type AdaptiveSmartSettings struct {
	Enabled                    bool   `json:"enabled"`
	FallbackStrategy           string `json:"fallbackStrategy"`
	CheckIntervalSec           int    `json:"checkIntervalSec"`
	CheckTimeoutSec            int    `json:"checkTimeoutSec"`
	MinScore                   int    `json:"minScore"`
	QuarantineAfterFailures    int    `json:"quarantineAfterFailures"`
	QuarantineDurationSec      int    `json:"quarantineDurationSec"`
	RecoveryWeight             int    `json:"recoveryWeight"`
	EnableThroughputProbe      bool   `json:"enableThroughputProbe"`
	ThroughputProbeSizeKB      int    `json:"throughputProbeSizeKB"`
	ThroughputProbeIntervalSec int    `json:"throughputProbeIntervalSec"`
	Debug                      bool   `json:"debug"`
	StrictMode                 bool   `json:"strictMode"`
}

type AdaptiveSmartMetadata map[string]AdaptiveSmartSettings

type AdaptiveSmartMemberReport struct {
	Tag                    string  `json:"tag"`
	Status                 string  `json:"status"`
	Score                  int     `json:"score"`
	WeightPercent          float64 `json:"weightPercent"`
	LatencyMs              int64   `json:"latencyMs"`
	SuccessRate            float64 `json:"successRate"`
	ConsecutiveFailures    int     `json:"consecutiveFailures"`
	TimeoutCount           int     `json:"timeoutCount"`
	ErrorCount             int     `json:"errorCount"`
	LastCheckedAt          int64   `json:"lastCheckedAt"`
	QuarantineRemainingSec int64   `json:"quarantineRemainingSec"`
	LastError              string  `json:"lastError,omitempty"`
	RuntimeEligible        bool    `json:"runtimeEligible"`
	RuntimeActive          bool    `json:"runtimeActive"`
	ExcludedReason         string  `json:"excludedReason,omitempty"`
}

type AdaptiveSmartHealthReport struct {
	Strategy                    string                      `json:"strategy"`
	Runtime                     string                      `json:"runtimeStrategy"`
	UpdatedAt                   int64                       `json:"updatedAt"`
	Members                     []AdaptiveSmartMemberReport `json:"members"`
	Events                      []AdaptiveSmartEvent        `json:"events,omitempty"`
	RuntimeSelector             []string                    `json:"runtimeSelector,omitempty"`
	PendingRuntimeSelector      []string                    `json:"pendingRuntimeSelector,omitempty"`
	RuntimeLastAppliedAt        int64                       `json:"runtimeLastAppliedAt,omitempty"`
	RuntimeCooldownRemainingSec int64                       `json:"runtimeCooldownRemainingSec,omitempty"`
	Message                     string                      `json:"message,omitempty"`
}

type AdaptiveSmartEvent struct {
	At        int64  `json:"at"`
	Level     string `json:"level"`
	Type      string `json:"type"`
	MemberTag string `json:"memberTag,omitempty"`
	Message   string `json:"message"`
}

type adaptiveSmartMemberState struct {
	LastLatencyMs       int64
	Successes           int
	Failures            int
	TimeoutCount        int
	ErrorCount          int
	ConsecutiveFailures int
	LastCheckedAt       time.Time
	QuarantinedUntil    time.Time
	RecoveryRemaining   int
	LastError           string
}

type adaptiveSmartBalancerState struct {
	UpdatedAt               time.Time
	Members                 map[string]*adaptiveSmartMemberState
	Report                  AdaptiveSmartHealthReport
	Events                  []AdaptiveSmartEvent
	RuntimeSelector         []string
	RuntimeSignature        string
	RuntimeAppliedAt        time.Time
	PendingRuntimeSelector  []string
	PendingRuntimeSignature string
	PendingRuntimeRepeats   int
}

type adaptiveBalancerDefinition struct {
	Tag       string
	Selectors []string
	Settings  AdaptiveSmartSettings
}

type adaptiveSmartEngine struct {
	mu             sync.Mutex
	running        bool
	stopCh         chan struct{}
	outboundSvc    OutboundService
	settingSvc     SettingService
	xraySettingSvc XraySettingService
	states         map[string]*adaptiveSmartBalancerState
}

var adaptiveEngine = &adaptiveSmartEngine{
	stopCh: make(chan struct{}),
	states: map[string]*adaptiveSmartBalancerState{},
}

func defaultAdaptiveSmartSettings() AdaptiveSmartSettings {
	return AdaptiveSmartSettings{
		Enabled:                    true,
		FallbackStrategy:           adaptiveSmartFallbackStrategy,
		CheckIntervalSec:           20,
		CheckTimeoutSec:            3,
		MinScore:                   40,
		QuarantineAfterFailures:    3,
		QuarantineDurationSec:      60,
		RecoveryWeight:             10,
		EnableThroughputProbe:      false,
		ThroughputProbeSizeKB:      128,
		ThroughputProbeIntervalSec: 180,
		Debug:                      false,
		StrictMode:                 false,
	}
}

func sanitizeAdaptiveSmartSettings(settings AdaptiveSmartSettings) AdaptiveSmartSettings {
	defaults := defaultAdaptiveSmartSettings()
	if settings.FallbackStrategy == "" || settings.FallbackStrategy == "adaptiveSmart" {
		settings.FallbackStrategy = defaults.FallbackStrategy
	}
	if settings.CheckIntervalSec <= 0 {
		settings.CheckIntervalSec = defaults.CheckIntervalSec
	}
	if settings.CheckTimeoutSec <= 0 {
		settings.CheckTimeoutSec = defaults.CheckTimeoutSec
	}
	if settings.MinScore <= 0 {
		settings.MinScore = defaults.MinScore
	}
	if settings.QuarantineAfterFailures <= 0 {
		settings.QuarantineAfterFailures = defaults.QuarantineAfterFailures
	}
	if settings.QuarantineDurationSec <= 0 {
		settings.QuarantineDurationSec = defaults.QuarantineDurationSec
	}
	if settings.RecoveryWeight <= 0 {
		settings.RecoveryWeight = defaults.RecoveryWeight
	}
	if settings.ThroughputProbeSizeKB <= 0 {
		settings.ThroughputProbeSizeKB = defaults.ThroughputProbeSizeKB
	}
	if settings.ThroughputProbeIntervalSec <= 0 {
		settings.ThroughputProbeIntervalSec = defaults.ThroughputProbeIntervalSec
	}
	if !settings.Enabled {
		settings.Enabled = true
	}
	return settings
}

func (s *SettingService) loadAdaptiveSmartMetadata() (AdaptiveSmartMetadata, error) {
	raw, err := s.GetAdaptiveSmartBalancers()
	if err != nil {
		return AdaptiveSmartMetadata{}, err
	}
	meta := AdaptiveSmartMetadata{}
	if strings.TrimSpace(raw) == "" {
		return meta, nil
	}
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		return AdaptiveSmartMetadata{}, err
	}
	for tag, cfg := range meta {
		meta[tag] = sanitizeAdaptiveSmartSettings(cfg)
	}
	return meta, nil
}

func (s *SettingService) saveAdaptiveSmartMetadata(meta AdaptiveSmartMetadata) error {
	if meta == nil {
		meta = AdaptiveSmartMetadata{}
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return s.SetAdaptiveSmartBalancers(string(raw))
}

func extractStrategyType(raw any) string {
	if m, ok := raw.(map[string]any); ok {
		if t, ok := m["type"].(string); ok {
			return t
		}
	}
	return ""
}

func parseAdaptiveSmartSettings(raw any) (AdaptiveSmartSettings, bool) {
	settings := defaultAdaptiveSmartSettings()
	if raw == nil {
		return settings, false
	}
	buf, err := json.Marshal(raw)
	if err != nil {
		return settings, false
	}
	if err := json.Unmarshal(buf, &settings); err != nil {
		return defaultAdaptiveSmartSettings(), false
	}
	return sanitizeAdaptiveSmartSettings(settings), true
}

func (s *XraySettingService) sanitizeAdaptiveSmartConfig(raw string) (string, error) {
	meta := AdaptiveSmartMetadata{}
	existing, _ := s.SettingService.loadAdaptiveSmartMetadata()
	for tag, cfg := range existing {
		meta[tag] = cfg
	}

	var top map[string]any
	if err := json.Unmarshal([]byte(raw), &top); err != nil {
		return raw, err
	}
	routing, _ := top["routing"].(map[string]any)
	if routing == nil {
		return raw, s.SettingService.saveAdaptiveSmartMetadata(AdaptiveSmartMetadata{})
	}
	balancers, _ := routing["balancers"].([]any)
	validTags := map[string]struct{}{}
	for _, entry := range balancers {
		balancer, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		tag, _ := balancer["tag"].(string)
		if tag == "" {
			continue
		}
		validTags[tag] = struct{}{}
		strategyType := extractStrategyType(balancer["strategy"])
		adaptiveCfg, hasAdaptiveCfg := parseAdaptiveSmartSettings(balancer["adaptiveSmart"])
		if strategyType == "adaptiveSmart" || hasAdaptiveCfg {
			if !hasAdaptiveCfg {
				if old, ok := meta[tag]; ok {
					adaptiveCfg = old
				} else {
					adaptiveCfg = defaultAdaptiveSmartSettings()
				}
			}
			meta[tag] = sanitizeAdaptiveSmartSettings(adaptiveCfg)
			balancer["strategy"] = map[string]any{"type": meta[tag].FallbackStrategy}
			delete(balancer, "adaptiveSmart")
			continue
		}
		delete(meta, tag)
	}
	for tag := range meta {
		if _, ok := validTags[tag]; !ok {
			delete(meta, tag)
		}
	}
	sanitized, err := json.Marshal(top)
	if err != nil {
		return raw, err
	}
	if err := s.SettingService.saveAdaptiveSmartMetadata(meta); err != nil {
		return raw, err
	}
	return string(sanitized), nil
}

func (s *XraySettingService) mergeAdaptiveSmartConfig(raw string) (string, error) {
	meta, err := s.SettingService.loadAdaptiveSmartMetadata()
	if err != nil || len(meta) == 0 {
		return raw, err
	}
	var top map[string]any
	if err := json.Unmarshal([]byte(raw), &top); err != nil {
		return raw, err
	}
	routing, _ := top["routing"].(map[string]any)
	if routing == nil {
		return raw, nil
	}
	balancers, _ := routing["balancers"].([]any)
	for _, entry := range balancers {
		balancer, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		tag, _ := balancer["tag"].(string)
		cfg, ok := meta[tag]
		if !ok {
			continue
		}
		balancer["strategy"] = map[string]any{"type": "adaptiveSmart"}
		balancer["adaptiveSmart"] = cfg
	}
	merged, err := json.Marshal(top)
	if err != nil {
		return raw, err
	}
	return string(merged), nil
}

func (e *adaptiveSmartEngine) EnsureRunning() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running {
		return
	}
	e.running = true
	go e.loop()
}

func (e *adaptiveSmartEngine) ApplyRuntimeRouting(raw []byte) ([]byte, error) {
	e.EnsureRunning()
	meta, err := e.settingSvc.loadAdaptiveSmartMetadata()
	if err != nil || len(meta) == 0 {
		return raw, err
	}

	var routing map[string]any
	if err := json.Unmarshal(raw, &routing); err != nil {
		return raw, err
	}
	balancers, _ := routing["balancers"].([]any)
	if len(balancers) == 0 {
		return raw, nil
	}
	for _, entry := range balancers {
		balancer, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		tag, _ := balancer["tag"].(string)
		if _, ok := meta[tag]; !ok {
			continue
		}
		runtimeSelector := e.getRuntimeSelector(tag)
		if len(runtimeSelector) == 0 {
			continue
		}
		selector := make([]any, 0, len(runtimeSelector))
		for _, memberTag := range runtimeSelector {
			selector = append(selector, memberTag)
		}
		balancer["selector"] = selector
	}
	merged, err := json.Marshal(routing)
	if err != nil {
		return raw, err
	}
	return json.RawMessage(merged), nil
}

func (e *adaptiveSmartEngine) loop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if _, err := e.refreshDue(); err != nil {
				logger.Debug("adaptive smart refresh skipped:", err)
			}
		case <-e.stopCh:
			return
		}
	}
}

func (e *adaptiveSmartEngine) GetReport(tag string, force bool) (*AdaptiveSmartHealthReport, error) {
	e.EnsureRunning()
	if force {
		if _, err := e.refreshBalancer(tag); err != nil {
			return nil, err
		}
	}
	if _, err := e.refreshDue(); err != nil {
		logger.Debug("adaptive smart due refresh warning:", err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	state := e.states[tag]
	if state == nil {
		return &AdaptiveSmartHealthReport{
			Strategy: "adaptiveSmart",
			Runtime:  adaptiveSmartFallbackStrategy,
			Message:  "No health data yet",
		}, nil
	}
	report := state.Report
	if len(report.Members) == 0 {
		report.Message = "No health data yet"
	}
	return &report, nil
}

func (e *adaptiveSmartEngine) refreshDue() (int, error) {
	defs, _, err := e.loadDefinitions()
	if err != nil {
		return 0, err
	}
	count := 0
	for _, def := range defs {
		if !e.shouldRefresh(def) {
			continue
		}
		if _, err := e.refreshBalancer(def.Tag); err == nil {
			count++
		}
	}
	return count, nil
}

func (e *adaptiveSmartEngine) shouldRefresh(def adaptiveBalancerDefinition) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	state := e.states[def.Tag]
	if state == nil || state.UpdatedAt.IsZero() {
		return true
	}
	interval := time.Duration(def.Settings.CheckIntervalSec) * time.Second
	jitterMs := crc32.ChecksumIEEE([]byte(def.Tag)) % 3000
	return time.Since(state.UpdatedAt) >= interval+time.Duration(jitterMs)*time.Millisecond
}

func (e *adaptiveSmartEngine) loadDefinitions() ([]adaptiveBalancerDefinition, map[string]map[string]any, error) {
	rawConfig, err := e.settingSvc.GetXrayConfigTemplate()
	if err != nil {
		return nil, nil, err
	}
	meta, err := e.settingSvc.loadAdaptiveSmartMetadata()
	if err != nil {
		return nil, nil, err
	}
	var top map[string]any
	if err := json.Unmarshal([]byte(rawConfig), &top); err != nil {
		return nil, nil, err
	}
	outbounds := map[string]map[string]any{}
	if outboundList, ok := top["outbounds"].([]any); ok {
		for _, row := range outboundList {
			outbound, ok := row.(map[string]any)
			if !ok {
				continue
			}
			tag, _ := outbound["tag"].(string)
			if tag == "" {
				continue
			}
			outbounds[tag] = outbound
		}
	}
	routing, _ := top["routing"].(map[string]any)
	balancerList, _ := routing["balancers"].([]any)
	defs := make([]adaptiveBalancerDefinition, 0, len(meta))
	for _, row := range balancerList {
		balancer, ok := row.(map[string]any)
		if !ok {
			continue
		}
		tag, _ := balancer["tag"].(string)
		cfg, ok := meta[tag]
		if !ok {
			continue
		}
		selectors := []string{}
		if rawSelectors, ok := balancer["selector"].([]any); ok {
			for _, sel := range rawSelectors {
				if s, ok := sel.(string); ok && s != "" {
					selectors = append(selectors, s)
				}
			}
		}
		defs = append(defs, adaptiveBalancerDefinition{
			Tag:       tag,
			Selectors: selectors,
			Settings:  cfg,
		})
	}
	return defs, outbounds, nil
}

func (e *adaptiveSmartEngine) refreshBalancer(tag string) (*AdaptiveSmartHealthReport, error) {
	defs, outbounds, err := e.loadDefinitions()
	if err != nil {
		return nil, err
	}
	var def *adaptiveBalancerDefinition
	for i := range defs {
		if defs[i].Tag == tag {
			def = &defs[i]
			break
		}
	}
	if def == nil {
		return &AdaptiveSmartHealthReport{
			Strategy: "adaptiveSmart",
			Runtime:  adaptiveSmartFallbackStrategy,
			Message:  "No health data yet",
		}, nil
	}
	allOutbounds := make([]any, 0, len(outbounds))
	for _, outbound := range outbounds {
		allOutbounds = append(allOutbounds, outbound)
	}
	allOutboundsJSON, _ := json.Marshal(allOutbounds)
	report := AdaptiveSmartHealthReport{
		Strategy:  "adaptiveSmart",
		Runtime:   def.Settings.FallbackStrategy,
		UpdatedAt: time.Now().UnixMilli(),
		Members:   make([]AdaptiveSmartMemberReport, 0, len(def.Selectors)),
	}
	e.mu.Lock()
	state := e.states[tag]
	if state == nil {
		state = &adaptiveSmartBalancerState{Members: map[string]*adaptiveSmartMemberState{}}
		e.states[tag] = state
	}
	e.mu.Unlock()

	for _, memberTag := range def.Selectors {
		memberState := e.ensureMemberState(tag, memberTag)
		outbound, ok := outbounds[memberTag]
		if !ok {
			report.Members = append(report.Members, AdaptiveSmartMemberReport{
				Tag:                    memberTag,
				Status:                 "unknown",
				Score:                  0,
				WeightPercent:          0,
				ConsecutiveFailures:    memberState.ConsecutiveFailures,
				TimeoutCount:           memberState.TimeoutCount,
				ErrorCount:             memberState.ErrorCount,
				LastCheckedAt:          memberState.LastCheckedAt.UnixMilli(),
				QuarantineRemainingSec: maxInt64(0, int64(time.Until(memberState.QuarantinedUntil).Seconds())),
			})
			continue
		}
		memberReport := e.probeMember(def.Settings, memberTag, outbound, string(allOutboundsJSON), memberState)
		report.Members = append(report.Members, memberReport)
	}
	assignAdaptiveWeights(report.Members, def.Settings)
	runtimeSelector := desiredAdaptiveRuntimeSelectors(report.Members, def.Selectors)
	pendingSelector := e.previewPendingRuntimeSelector(tag)
	markAdaptiveRuntimeMembership(report.Members, runtimeSelector, pendingSelector)
	sort.Slice(report.Members, func(i, j int) bool {
		if report.Members[i].WeightPercent == report.Members[j].WeightPercent {
			return report.Members[i].Tag < report.Members[j].Tag
		}
		return report.Members[i].WeightPercent > report.Members[j].WeightPercent
	})

	e.mu.Lock()
	state.UpdatedAt = time.Now()
	report.Events = append([]AdaptiveSmartEvent(nil), state.Events...)
	report.RuntimeSelector = append([]string(nil), state.RuntimeSelector...)
	report.PendingRuntimeSelector = append([]string(nil), state.PendingRuntimeSelector...)
	report.RuntimeLastAppliedAt = state.RuntimeAppliedAt.UnixMilli()
	report.RuntimeCooldownRemainingSec = maxInt64(0, int64(adaptiveSmartRuntimeCooldown.Seconds())-int64(time.Since(state.RuntimeAppliedAt).Seconds()))
	state.Report = report
	e.mu.Unlock()
	if e.commitRuntimeSelector(tag, runtimeSelector) {
		(&XrayService{}).SetToNeedRestart()
	}
	return &report, nil
}

func (e *adaptiveSmartEngine) ensureMemberState(balancerTag, memberTag string) *adaptiveSmartMemberState {
	e.mu.Lock()
	defer e.mu.Unlock()
	state := e.states[balancerTag]
	if state == nil {
		state = &adaptiveSmartBalancerState{Members: map[string]*adaptiveSmartMemberState{}}
		e.states[balancerTag] = state
	}
	member := state.Members[memberTag]
	if member == nil {
		member = &adaptiveSmartMemberState{}
		state.Members[memberTag] = member
	}
	return member
}

func (e *adaptiveSmartEngine) getRuntimeSelector(tag string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	state := e.states[tag]
	if state == nil || len(state.RuntimeSelector) == 0 {
		return nil
	}
	return append([]string(nil), state.RuntimeSelector...)
}

func (e *adaptiveSmartEngine) previewPendingRuntimeSelector(tag string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	state := e.states[tag]
	if state == nil || len(state.PendingRuntimeSelector) == 0 {
		return nil
	}
	return append([]string(nil), state.PendingRuntimeSelector...)
}

func (e *adaptiveSmartEngine) appendEvent(tag string, event AdaptiveSmartEvent) {
	e.mu.Lock()
	defer e.mu.Unlock()
	state := e.states[tag]
	if state == nil {
		state = &adaptiveSmartBalancerState{Members: map[string]*adaptiveSmartMemberState{}}
		e.states[tag] = state
	}
	state.Events = append(state.Events, event)
	if len(state.Events) > adaptiveSmartEventHistorySize {
		state.Events = state.Events[len(state.Events)-adaptiveSmartEventHistorySize:]
	}
}

func (e *adaptiveSmartEngine) commitRuntimeSelector(tag string, desired []string) bool {
	return e.commitRuntimeSelectorAt(tag, desired, time.Now())
}

func (e *adaptiveSmartEngine) commitRuntimeSelectorAt(tag string, desired []string, now time.Time) bool {
	signature := adaptiveRuntimeSignature(desired)
	e.mu.Lock()
	defer e.mu.Unlock()
	state := e.states[tag]
	if state == nil {
		return false
	}
	if signature == state.RuntimeSignature {
		state.PendingRuntimeSignature = ""
		state.PendingRuntimeSelector = nil
		state.PendingRuntimeRepeats = 0
		return false
	}
	if !state.RuntimeAppliedAt.IsZero() && now.Sub(state.RuntimeAppliedAt) < adaptiveSmartRuntimeCooldown {
		cooldownRemaining := adaptiveSmartRuntimeCooldown - now.Sub(state.RuntimeAppliedAt)
		state.PendingRuntimeSignature = signature
		state.PendingRuntimeSelector = append([]string(nil), desired...)
		state.PendingRuntimeRepeats = 0
		go e.appendEvent(tag, AdaptiveSmartEvent{
			At:      now.UnixMilli(),
			Level:   "info",
			Type:    "cooldown",
			Message: "Runtime selector change held by cooldown for " + cooldownRemaining.Round(time.Second).String(),
		})
		return false
	}
	if signature != state.PendingRuntimeSignature {
		state.PendingRuntimeSignature = signature
		state.PendingRuntimeSelector = append([]string(nil), desired...)
		state.PendingRuntimeRepeats = 1
		go e.appendEvent(tag, AdaptiveSmartEvent{
			At:      now.UnixMilli(),
			Level:   "info",
			Type:    "pending",
			Message: "Observed new runtime selector candidate: " + strings.Join(desired, ", "),
		})
		return false
	}

	state.PendingRuntimeRepeats++
	requiredRepeats := adaptiveRuntimeRequiredRepeats(state.RuntimeSelector, desired)
	if state.PendingRuntimeRepeats < requiredRepeats {
		return false
	}
	state.RuntimeSignature = signature
	state.RuntimeSelector = append([]string(nil), state.PendingRuntimeSelector...)
	state.RuntimeAppliedAt = now
	state.PendingRuntimeSignature = ""
	state.PendingRuntimeSelector = nil
	state.PendingRuntimeRepeats = 0
	go e.appendEvent(tag, AdaptiveSmartEvent{
		At:      now.UnixMilli(),
		Level:   "warning",
		Type:    "runtime_apply",
		Message: "Applied runtime selector: " + strings.Join(state.RuntimeSelector, ", "),
	})
	return true
}

func (e *adaptiveSmartEngine) probeMember(settings AdaptiveSmartSettings, tag string, outbound map[string]any, allOutboundsJSON string, memberState *adaptiveSmartMemberState) AdaptiveSmartMemberReport {
	now := time.Now()
	report := AdaptiveSmartMemberReport{
		Tag: tag,
	}
	statusBefore := adaptiveMemberStatus(memberState, settings, now, adaptiveScore(memberState, settings, now, false))
	remaining := int64(math.Max(0, time.Until(memberState.QuarantinedUntil).Seconds()))
	if remaining > 0 {
		score := adaptiveScore(memberState, settings, now, true)
		report.Status = "quarantined"
		report.Score = score
		report.LatencyMs = memberState.LastLatencyMs
		report.SuccessRate = adaptiveSuccessRate(memberState)
		report.ConsecutiveFailures = memberState.ConsecutiveFailures
		report.TimeoutCount = memberState.TimeoutCount
		report.ErrorCount = memberState.ErrorCount
		report.LastCheckedAt = memberState.LastCheckedAt.UnixMilli()
		report.QuarantineRemainingSec = remaining
		return report
	}

	outboundJSON, _ := json.Marshal(outbound)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(settings.CheckTimeoutSec+2)*time.Second)
	defer cancel()
	testRes, err := e.outboundSvc.TestOutboundContext(ctx, string(outboundJSON), mustOutboundTestURL(e.settingSvc), allOutboundsJSON)
	memberState.LastCheckedAt = now
	if err != nil {
		memberState.Failures++
		memberState.ConsecutiveFailures++
		memberState.LastError = err.Error()
		if ctx.Err() != nil {
			memberState.TimeoutCount++
			memberState.ErrorCount++
		}
	} else if testRes != nil && testRes.Success {
		memberState.Successes++
		memberState.LastLatencyMs = testRes.Delay
		memberState.LastError = ""
		memberState.ConsecutiveFailures = 0
		if memberState.RecoveryRemaining > 0 {
			memberState.RecoveryRemaining--
		}
	} else {
		memberState.Failures++
		memberState.ConsecutiveFailures++
		memberState.LastError = "unknown"
		if testRes != nil {
			memberState.LastError = testRes.Error
			if strings.Contains(strings.ToLower(testRes.Error), "timeout") || strings.Contains(strings.ToLower(testRes.Error), "context deadline exceeded") {
				memberState.TimeoutCount++
			}
			if strings.Contains(strings.ToLower(testRes.Error), "reset") || strings.Contains(strings.ToLower(testRes.Error), "eof") || strings.Contains(strings.ToLower(testRes.Error), "canceled") {
				memberState.ErrorCount++
			}
		}
		if ctx.Err() != nil {
			memberState.TimeoutCount++
			memberState.ErrorCount++
		}
	}
	if memberState.ConsecutiveFailures >= settings.QuarantineAfterFailures {
		memberState.QuarantinedUntil = now.Add(time.Duration(settings.QuarantineDurationSec) * time.Second)
		memberState.RecoveryRemaining = 2
	}
	score := adaptiveScore(memberState, settings, now, false)
	report.Status = adaptiveMemberStatus(memberState, settings, now, score)
	report.Score = score
	report.LatencyMs = memberState.LastLatencyMs
	report.SuccessRate = adaptiveSuccessRate(memberState)
	report.ConsecutiveFailures = memberState.ConsecutiveFailures
	report.TimeoutCount = memberState.TimeoutCount
	report.ErrorCount = memberState.ErrorCount
	report.LastCheckedAt = memberState.LastCheckedAt.UnixMilli()
	report.QuarantineRemainingSec = maxInt64(0, int64(time.Until(memberState.QuarantinedUntil).Seconds()))
	report.LastError = memberState.LastError
	e.recordMemberTransition(report.Tag, statusBefore, report.Status, report.LastError, report.QuarantineRemainingSec, now)
	return report
}

func (e *adaptiveSmartEngine) recordMemberTransition(memberTag, before, after, lastError string, quarantineRemaining int64, now time.Time) {
	if before == after {
		return
	}
	event := AdaptiveSmartEvent{
		At:        now.UnixMilli(),
		Level:     "info",
		Type:      "member_status",
		MemberTag: memberTag,
		Message:   "Status changed from " + before + " to " + after,
	}
	switch after {
	case "quarantined":
		event.Level = "warning"
		event.Message = "Member quarantined for " + (time.Duration(quarantineRemaining) * time.Second).Round(time.Second).String()
	case "unhealthy":
		event.Level = "warning"
		if lastError != "" {
			event.Message = "Member became unhealthy: " + lastError
		}
	case "recovering":
		event.Message = "Member left quarantine and is recovering"
	case "healthy":
		event.Message = "Member is healthy again"
	}
	e.appendEventForMember(memberTag, event)
}

func (e *adaptiveSmartEngine) appendEventForMember(memberTag string, event AdaptiveSmartEvent) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for balancerTag, state := range e.states {
		if state == nil {
			continue
		}
		if _, ok := state.Members[memberTag]; !ok {
			continue
		}
		state.Events = append(state.Events, event)
		if len(state.Events) > adaptiveSmartEventHistorySize {
			state.Events = state.Events[len(state.Events)-adaptiveSmartEventHistorySize:]
		}
		e.states[balancerTag] = state
	}
}

func mustOutboundTestURL(settingSvc SettingService) string {
	if testURL, err := settingSvc.GetXrayOutboundTestUrl(); err == nil && testURL != "" {
		return testURL
	}
	return "https://www.google.com/generate_204"
}

func adaptiveSuccessRate(state *adaptiveSmartMemberState) float64 {
	total := state.Successes + state.Failures
	if total <= 0 {
		return 0
	}
	return float64(state.Successes) / float64(total)
}

func adaptiveScore(state *adaptiveSmartMemberState, settings AdaptiveSmartSettings, now time.Time, forceQuarantined bool) int {
	if forceQuarantined || now.Before(state.QuarantinedUntil) {
		return 0
	}
	score := 100
	if state.LastLatencyMs > 0 {
		score -= int(minFloat(35, float64(state.LastLatencyMs)/40.0))
	}
	score -= minInt(25, state.TimeoutCount*8)
	score -= minInt(20, state.ErrorCount*4)
	score -= minInt(30, state.ConsecutiveFailures*12)
	successRate := adaptiveSuccessRate(state)
	if state.Successes+state.Failures > 0 {
		score -= int((1.0 - successRate) * 25.0)
	}
	if successRate >= 0.9 && state.Successes >= 3 {
		score += 5
	}
	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}
	return score
}

func adaptiveMemberStatus(state *adaptiveSmartMemberState, settings AdaptiveSmartSettings, now time.Time, score int) string {
	switch {
	case now.Before(state.QuarantinedUntil):
		return "quarantined"
	case state.LastCheckedAt.IsZero():
		return "unknown"
	case state.RecoveryRemaining > 0:
		return "recovering"
	case score < settings.MinScore:
		return "unhealthy"
	default:
		return "healthy"
	}
}

func assignAdaptiveWeights(members []AdaptiveSmartMemberReport, settings AdaptiveSmartSettings) {
	if len(members) == 0 {
		return
	}
	rawWeights := make([]float64, len(members))
	total := 0.0
	bestIdx := 0
	bestScore := -1
	for i, member := range members {
		if member.Score > bestScore {
			bestScore = member.Score
			bestIdx = i
		}
		switch member.Status {
		case "quarantined":
			rawWeights[i] = 0
		case "recovering":
			rawWeights[i] = float64(settings.RecoveryWeight)
		default:
			if member.Score >= settings.MinScore {
				rawWeights[i] = float64(maxInt(1, member.Score-settings.MinScore+1))
			}
		}
		total += rawWeights[i]
	}
	if total == 0 {
		if settings.StrictMode {
			return
		}
		rawWeights[bestIdx] = 1
		total = 1
	}
	for i := range members {
		members[i].WeightPercent = math.Round((rawWeights[i]/total)*1000) / 10
	}
}

func desiredAdaptiveRuntimeSelectors(members []AdaptiveSmartMemberReport, originalSelectors []string) []string {
	if len(originalSelectors) == 0 {
		return nil
	}
	memberByTag := make(map[string]AdaptiveSmartMemberReport, len(members))
	bestTag := ""
	bestScore := -1
	for _, member := range members {
		memberByTag[member.Tag] = member
		if member.Status != "quarantined" && member.Score > bestScore {
			bestScore = member.Score
			bestTag = member.Tag
		}
	}
	selected := make([]string, 0, len(originalSelectors))
	for _, tag := range originalSelectors {
		member, ok := memberByTag[tag]
		if !ok {
			selected = append(selected, tag)
			continue
		}
		switch member.Status {
		case "healthy", "recovering", "unknown":
			selected = append(selected, tag)
		}
	}
	if len(selected) > 0 {
		return selected
	}
	if bestTag != "" {
		return []string{bestTag}
	}
	return append([]string(nil), originalSelectors...)
}

func markAdaptiveRuntimeMembership(members []AdaptiveSmartMemberReport, runtimeSelector, pendingSelector []string) {
	activeSet := make(map[string]struct{}, len(runtimeSelector))
	for _, tag := range runtimeSelector {
		activeSet[tag] = struct{}{}
	}
	pendingSet := make(map[string]struct{}, len(pendingSelector))
	for _, tag := range pendingSelector {
		pendingSet[tag] = struct{}{}
	}
	for i := range members {
		_, members[i].RuntimeEligible = pendingSet[members[i].Tag]
		_, members[i].RuntimeActive = activeSet[members[i].Tag]
		if members[i].RuntimeEligible || members[i].RuntimeActive {
			continue
		}
		switch members[i].Status {
		case "quarantined":
			members[i].ExcludedReason = "quarantined"
		case "unhealthy":
			members[i].ExcludedReason = "score below minimum"
		default:
			members[i].ExcludedReason = "waiting for stable health signal"
		}
	}
}

func adaptiveRuntimeSignature(selectors []string) string {
	return strings.Join(selectors, ",")
}

func adaptiveRuntimeRequiredRepeats(current, desired []string) int {
	if len(current) == 0 {
		return 2
	}
	if len(desired) < len(current) {
		return 3
	}
	return 2
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

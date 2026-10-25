package service

import (
	"testing"
	"time"
)

func TestAdaptiveScoreDropsWithFailures(t *testing.T) {
	settings := defaultAdaptiveSmartSettings()
	healthy := &adaptiveSmartMemberState{Successes: 5, LastLatencyMs: 80}
	unhealthy := &adaptiveSmartMemberState{
		Successes:           1,
		Failures:            4,
		LastLatencyMs:       900,
		TimeoutCount:        2,
		ErrorCount:          2,
		ConsecutiveFailures: 3,
	}
	now := time.Now()
	if got, want := adaptiveScore(healthy, settings, now, false), adaptiveScore(unhealthy, settings, now, false); got <= want {
		t.Fatalf("expected healthy score > unhealthy score, got healthy=%d unhealthy=%d", got, want)
	}
}

func TestAdaptiveQuarantineStatus(t *testing.T) {
	settings := defaultAdaptiveSmartSettings()
	state := &adaptiveSmartMemberState{
		QuarantinedUntil: time.Now().Add(30 * time.Second),
	}
	if got := adaptiveMemberStatus(state, settings, time.Now(), 0); got != "quarantined" {
		t.Fatalf("expected quarantined, got %q", got)
	}
}

func TestAdaptiveRecoveryStatus(t *testing.T) {
	settings := defaultAdaptiveSmartSettings()
	state := &adaptiveSmartMemberState{
		LastCheckedAt:     time.Now(),
		RecoveryRemaining: 1,
	}
	if got := adaptiveMemberStatus(state, settings, time.Now(), 80); got != "recovering" {
		t.Fatalf("expected recovering, got %q", got)
	}
}

func TestAdaptiveWeightsFallbackToLeastBad(t *testing.T) {
	settings := defaultAdaptiveSmartSettings()
	settings.StrictMode = false
	members := []AdaptiveSmartMemberReport{
		{Tag: "a", Status: "unhealthy", Score: 12},
		{Tag: "b", Status: "unhealthy", Score: 8},
	}
	assignAdaptiveWeights(members, settings)
	if members[0].WeightPercent == 0 && members[1].WeightPercent == 0 {
		t.Fatal("expected at least one fallback weight when strict mode is disabled")
	}
}

func TestAdaptiveWeightsStrictModeCanZeroAll(t *testing.T) {
	settings := defaultAdaptiveSmartSettings()
	settings.StrictMode = true
	members := []AdaptiveSmartMemberReport{
		{Tag: "a", Status: "unhealthy", Score: 12},
		{Tag: "b", Status: "unhealthy", Score: 8},
	}
	assignAdaptiveWeights(members, settings)
	if members[0].WeightPercent != 0 || members[1].WeightPercent != 0 {
		t.Fatal("expected all weights to remain zero in strict mode")
	}
}

func TestDesiredAdaptiveRuntimeSelectorsFiltersUnhealthyMembers(t *testing.T) {
	members := []AdaptiveSmartMemberReport{
		{Tag: "a", Status: "healthy", Score: 80},
		{Tag: "b", Status: "unhealthy", Score: 20},
		{Tag: "c", Status: "recovering", Score: 55},
		{Tag: "d", Status: "quarantined", Score: 0},
	}
	got := desiredAdaptiveRuntimeSelectors(members, []string{"a", "b", "c", "d"})
	want := []string{"a", "c"}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expected %v, got %v", want, got)
		}
	}
}

func TestDesiredAdaptiveRuntimeSelectorsFallsBackToBestMember(t *testing.T) {
	members := []AdaptiveSmartMemberReport{
		{Tag: "a", Status: "unhealthy", Score: 35},
		{Tag: "b", Status: "unhealthy", Score: 10},
		{Tag: "c", Status: "quarantined", Score: 0},
	}
	got := desiredAdaptiveRuntimeSelectors(members, []string{"a", "b", "c"})
	if len(got) != 1 || got[0] != "a" {
		t.Fatalf("expected best fallback selector [a], got %v", got)
	}
}

func TestCommitRuntimeSelectorRequiresStableRefresh(t *testing.T) {
	engine := &adaptiveSmartEngine{
		states: map[string]*adaptiveSmartBalancerState{
			"bal": {
				Members: map[string]*adaptiveSmartMemberState{},
			},
		},
	}
	now := time.Now()
	if changed := engine.commitRuntimeSelectorAt("bal", []string{"a", "b"}, now); changed {
		t.Fatal("expected first selector change to stay pending")
	}
	if changed := engine.commitRuntimeSelectorAt("bal", []string{"a", "b"}, now); !changed {
		t.Fatal("expected second identical selector change to commit")
	}
	got := engine.getRuntimeSelector("bal")
	want := []string{"a", "b"}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expected %v, got %v", want, got)
		}
	}
}

func TestCommitRuntimeSelectorShrinkNeedsExtraRepeat(t *testing.T) {
	engine := &adaptiveSmartEngine{
		states: map[string]*adaptiveSmartBalancerState{
			"bal": {
				Members:          map[string]*adaptiveSmartMemberState{},
				RuntimeSelector:  []string{"a", "b", "c"},
				RuntimeSignature: adaptiveRuntimeSignature([]string{"a", "b", "c"}),
			},
		},
	}
	now := time.Now()
	for i := 0; i < 2; i++ {
		if changed := engine.commitRuntimeSelectorAt("bal", []string{"a", "b"}, now.Add(time.Duration(i)*time.Second)); changed {
			t.Fatal("expected shrink to stay pending until third identical refresh")
		}
	}
	if changed := engine.commitRuntimeSelectorAt("bal", []string{"a", "b"}, now.Add(2*time.Second)); !changed {
		t.Fatal("expected shrink to commit on third identical refresh")
	}
}

func TestCommitRuntimeSelectorRespectsCooldown(t *testing.T) {
	engine := &adaptiveSmartEngine{
		states: map[string]*adaptiveSmartBalancerState{
			"bal": {
				Members:          map[string]*adaptiveSmartMemberState{},
				RuntimeSelector:  []string{"a", "b"},
				RuntimeSignature: adaptiveRuntimeSignature([]string{"a", "b"}),
				RuntimeAppliedAt: time.Now(),
			},
		},
	}
	state := engine.states["bal"]
	if changed := engine.commitRuntimeSelectorAt("bal", []string{"a", "b", "c"}, state.RuntimeAppliedAt.Add(time.Second)); changed {
		t.Fatal("expected cooldown to block early runtime change")
	}
	if changed := engine.commitRuntimeSelectorAt("bal", []string{"a", "b", "c"}, state.RuntimeAppliedAt.Add(adaptiveSmartRuntimeCooldown+time.Second)); changed {
		t.Fatal("expected first post-cooldown refresh to remain pending")
	}
	if changed := engine.commitRuntimeSelectorAt("bal", []string{"a", "b", "c"}, state.RuntimeAppliedAt.Add(adaptiveSmartRuntimeCooldown+2*time.Second)); !changed {
		t.Fatal("expected second stable refresh after cooldown to commit")
	}
}

func TestMarkAdaptiveRuntimeMembershipAnnotatesMembers(t *testing.T) {
	members := []AdaptiveSmartMemberReport{
		{Tag: "a", Status: "healthy"},
		{Tag: "b", Status: "unhealthy"},
		{Tag: "c", Status: "quarantined"},
	}
	markAdaptiveRuntimeMembership(members, []string{"a"}, []string{"a", "b"})
	if !members[0].RuntimeActive || !members[0].RuntimeEligible {
		t.Fatal("expected member a to be active and eligible")
	}
	if members[1].RuntimeActive || !members[1].RuntimeEligible {
		t.Fatal("expected member b to be pending-eligible only")
	}
	if members[2].ExcludedReason != "quarantined" {
		t.Fatalf("expected quarantined reason, got %q", members[2].ExcludedReason)
	}
}

func TestAppendEventCapsHistory(t *testing.T) {
	engine := &adaptiveSmartEngine{
		states: map[string]*adaptiveSmartBalancerState{
			"bal": {Members: map[string]*adaptiveSmartMemberState{}},
		},
	}
	for i := 0; i < adaptiveSmartEventHistorySize+5; i++ {
		engine.appendEvent("bal", AdaptiveSmartEvent{
			At:      int64(i),
			Level:   "info",
			Type:    "test",
			Message: "event",
		})
	}
	events := engine.states["bal"].Events
	if len(events) != adaptiveSmartEventHistorySize {
		t.Fatalf("expected %d events, got %d", adaptiveSmartEventHistorySize, len(events))
	}
	if events[0].At != 5 {
		t.Fatalf("expected oldest retained event to be 5, got %d", events[0].At)
	}
}

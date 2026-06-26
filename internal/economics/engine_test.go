package economics

import (
	"testing"

	"github.com/clawdbot/keystone/internal/classify"
	"github.com/clawdbot/keystone/internal/config"
	"github.com/clawdbot/keystone/internal/keypool"
	"github.com/clawdbot/keystone/internal/session"
)

func newTestConfig() *config.Config {
	return &config.Config{
		AgentTiers: map[string]string{
			"plan": "mid",
		},
		Tiers: map[string]config.TierConfig{
			"free":    {Models: []string{"gemma-4-31b", "llama-70b"}},
			"mid":     {Models: []string{"nemotron-super"}},
			"coder":   {Models: []string{"qwen3-coder"}},
			"premium": {Models: []string{"glm-5.1", "nemotron-ultra"}},
		},
		Economics: config.EconomicsConfig{
			StickyMinTurns:   3,
			StickyMinContext: 5000,
			CacheHitRatio:    0.7,
		},
	}
}

func healthySession() *session.Session {
	return &session.Session{
		Key:       &keypool.Key{State: keypool.StateHealthy},
		Tier:      "premium",
		TurnCount: 5,
	}
}

func TestDecide_BasicRouting(t *testing.T) {
	e := New(newTestConfig())
	c := &classify.Result{TaskType: "conversation", Complexity: "moderate", ContextType: "new_session"}
	dec := e.Decide(c, nil, "default", "")
	if dec.Tier != "mid" {
		t.Fatalf("expected tier mid for moderate conversation, got %s", dec.Tier)
	}
	if dec.Sticky {
		t.Fatal("expected no sticky with nil session")
	}
}

func TestDecide_TrivialStandalone(t *testing.T) {
	e := New(newTestConfig())
	c := &classify.Result{TaskType: "simple_query", Complexity: "trivial", ContextType: "standalone"}
	dec := e.Decide(c, nil, "default", "")
	if dec.Tier != "free" {
		t.Fatalf("expected free for trivial, got %s", dec.Tier)
	}
}

func TestDecide_SimpleCoding(t *testing.T) {
	e := New(newTestConfig())
	c := &classify.Result{TaskType: "coding", Complexity: "simple", ContextType: "new_session"}
	dec := e.Decide(c, nil, "default", "")
	if dec.Tier != "coder" {
		t.Fatalf("expected coder for simple coding, got %s", dec.Tier)
	}
}

func TestDecide_ModerateCoding(t *testing.T) {
	e := New(newTestConfig())
	c := &classify.Result{TaskType: "coding", Complexity: "moderate", ContextType: "new_session"}
	dec := e.Decide(c, nil, "default", "")
	if dec.Tier != "coder" {
		t.Fatalf("expected coder for moderate coding, got %s", dec.Tier)
	}
}

func TestDecide_ComplexTask(t *testing.T) {
	e := New(newTestConfig())
	c := &classify.Result{TaskType: "coding", Complexity: "complex", ContextType: "new_session"}
	dec := e.Decide(c, nil, "default", "")
	if dec.Tier != "premium" {
		t.Fatalf("expected premium for complex, got %s", dec.Tier)
	}
}

func TestDecide_ExpertTask(t *testing.T) {
	e := New(newTestConfig())
	c := &classify.Result{TaskType: "reasoning", Complexity: "expert", ContextType: "new_session"}
	dec := e.Decide(c, nil, "default", "")
	if dec.Tier != "premium" {
		t.Fatalf("expected premium for expert, got %s", dec.Tier)
	}
}

func TestAgentFloor_RaisesTier(t *testing.T) {
	e := New(newTestConfig())
	// Trivial conversation with plan agent (mid floor)
	c := &classify.Result{TaskType: "conversation", Complexity: "trivial", ContextType: "standalone"}
	dec := e.Decide(c, nil, "plan", "")
	if dec.Tier != "mid" {
		t.Fatalf("expected mid (agent floor), got %s", dec.Tier)
	}
}

func TestAgentFloor_NoEffectWhenBelow(t *testing.T) {
	e := New(newTestConfig())
	// Moderate conversation with plan agent (mid floor) — moderate is already mid
	c := &classify.Result{TaskType: "conversation", Complexity: "moderate", ContextType: "new_session"}
	dec := e.Decide(c, nil, "plan", "")
	if dec.Tier != "mid" {
		t.Fatalf("expected mid unchanged, got %s", dec.Tier)
	}
}

func TestAgentFloor_NoEffectForUnknownAgent(t *testing.T) {
	e := New(newTestConfig())
	c := &classify.Result{TaskType: "simple_query", Complexity: "trivial", ContextType: "standalone"}
	dec := e.Decide(c, nil, "unknown-agent", "")
	if dec.Tier != "free" {
		t.Fatalf("expected free for unknown agent, got %s", dec.Tier)
	}
}

func TestModelFloor_RaisesTier(t *testing.T) {
	e := New(newTestConfig())
	// Trivial task with explicit premium model request
	c := &classify.Result{TaskType: "simple_query", Complexity: "trivial", ContextType: "standalone"}
	dec := e.Decide(c, nil, "default", "glm-5.1")
	if dec.Tier != "premium" {
		t.Fatalf("expected premium (model floor), got %s", dec.Tier)
	}
}

func TestModelFloor_NotAppliedForAuto(t *testing.T) {
	e := New(newTestConfig())
	c := &classify.Result{TaskType: "conversation", Complexity: "moderate", ContextType: "new_session"}
	dec := e.Decide(c, nil, "default", "auto")
	if dec.Tier != "mid" {
		t.Fatalf("expected mid (auto ignores model floor), got %s", dec.Tier)
	}
}

func TestModelFloor_NotAppliedForEmpty(t *testing.T) {
	e := New(newTestConfig())
	c := &classify.Result{TaskType: "conversation", Complexity: "moderate", ContextType: "new_session"}
	dec := e.Decide(c, nil, "default", "")
	if dec.Tier != "mid" {
		t.Fatalf("expected mid (empty ignores model floor), got %s", dec.Tier)
	}
}

func TestSessionContinuation_StaysSticky(t *testing.T) {
	e := New(newTestConfig())
	s := healthySession()
	c := &classify.Result{TaskType: "coding", Complexity: "moderate", ContextType: "session_continuation"}
	dec := e.Decide(c, s, "default", "")
	if !dec.Sticky {
		t.Fatal("expected sticky for session continuation")
	}
}

func TestTrivialStandalone_NotSticky(t *testing.T) {
	e := New(newTestConfig())
	s := healthySession()
	c := &classify.Result{TaskType: "simple_query", Complexity: "trivial", ContextType: "standalone"}
	dec := e.Decide(c, s, "default", "")
	if dec.Sticky {
		t.Fatal("expected no sticky for trivial standalone")
	}
}

func TestDowngrade_Detected(t *testing.T) {
	e := New(newTestConfig())
	s := healthySession() // current tier = premium
	c := &classify.Result{TaskType: "simple_query", Complexity: "trivial", ContextType: "standalone"}
	dec := e.Decide(c, s, "default", "")
	if !dec.Downgrade {
		t.Fatal("expected downgrade detected (premium -> free)")
	}
}

func TestDowngrade_NotDetectedWhenSame(t *testing.T) {
	e := New(newTestConfig())
	s := healthySession() // current tier = premium
	c := &classify.Result{TaskType: "coding", Complexity: "expert", ContextType: "session_continuation"}
	dec := e.Decide(c, s, "default", "")
	if dec.Downgrade {
		t.Fatal("expected no downgrade (premium -> premium)")
	}
}

func TestSticky_PreventsDowngradeWithEnoughTurns(t *testing.T) {
	e := New(newTestConfig())
	s := healthySession() // premium, 5 turns
	c := &classify.Result{TaskType: "simple_query", Complexity: "trivial", ContextType: "new_session"}
	// target tier = free, but sticky should keep them on premium
	dec := e.Decide(c, s, "default", "")
	if !dec.Sticky {
		t.Fatal("expected sticky to prevent downgrade at >=3 turns")
	}
	if dec.Tier != "free" {
		t.Fatalf("expected tier free (target), got %s", dec.Tier)
	}
}

func TestSticky_AllowsDowngradeWithFewTurns(t *testing.T) {
	e := New(newTestConfig())
	s := &session.Session{
		Key:       &keypool.Key{State: keypool.StateHealthy},
		Tier:      "premium",
		TurnCount: 1,
	}
	c := &classify.Result{TaskType: "simple_query", Complexity: "trivial", ContextType: "new_session"}
	dec := e.Decide(c, s, "default", "")
	if dec.Sticky {
		t.Fatal("expected no sticky at 1 turn (< min 3)")
	}
	if !dec.Downgrade {
		t.Fatal("expected downgrade to be allowed")
	}
}

func TestShouldStaySticky_NilSession(t *testing.T) {
	e := New(newTestConfig())
	if e.shouldStaySticky(&classify.Result{}, nil, "premium") {
		t.Fatal("expected false for nil session")
	}
}

func TestShouldStaySticky_NilKey(t *testing.T) {
	e := New(newTestConfig())
	s := &session.Session{}
	if e.shouldStaySticky(&classify.Result{}, s, "premium") {
		t.Fatal("expected false for nil key")
	}
}

func TestDetermineComplexityTier(t *testing.T) {
	e := New(newTestConfig())
	cases := []struct {
		taskType   string
		complexity string
		want       string
	}{
		{"simple_query", "trivial", "free"},
		{"conversation", "trivial", "free"},
		{"coding", "trivial", "free"},
		{"conversation", "simple", "free"},
		{"coding", "simple", "coder"},
		{"data_extraction", "simple", "free"},
		{"conversation", "moderate", "mid"},
		{"coding", "moderate", "coder"},
		{"coding", "complex", "premium"},
		{"conversation", "complex", "premium"},
		{"reasoning", "expert", "premium"},
		{"conversation", "unknown", "mid"},
	}
	for _, tc := range cases {
		c := &classify.Result{TaskType: tc.taskType, Complexity: tc.complexity}
		if got := e.determineComplexityTier(c); got != tc.want {
			t.Errorf("determineComplexityTier(%s,%s) = %s, want %s", tc.taskType, tc.complexity, got, tc.want)
		}
	}
}

func TestTierRank(t *testing.T) {
	cases := []struct{ tier string; rank int }{
		{"free", 0},
		{"mid", 1},
		{"coder", 2},
		{"premium", 3},
		{"unknown", 1},
	}
	for _, tc := range cases {
		if got := tierRank(tc.tier); got != tc.rank {
			t.Errorf("tierRank(%q) = %d, want %d", tc.tier, got, tc.rank)
		}
	}
}

func TestTierFromRank(t *testing.T) {
	cases := []struct {
		rank int
		want string
	}{
		{0, "free"},
		{1, "mid"},
		{2, "coder"},
		{3, "premium"},
		{99, "mid"},
	}
	for _, tc := range cases {
		if got := tierFromRank(tc.rank); got != tc.want {
			t.Errorf("tierFromRank(%d) = %s, want %s", tc.rank, got, tc.want)
		}
	}
}

func TestModelTier(t *testing.T) {
	e := New(newTestConfig())
	cases := []struct {
		model string
		want  string
	}{
		{"gemma-4-31b", "free"},
		{"llama-70b", "free"},
		{"nemotron-super", "mid"},
		{"qwen3-coder", "coder"},
		{"glm-5.1", "premium"},
		{"nemotron-ultra", "premium"},
		{"unknown-model", ""},
	}
	for _, tc := range cases {
		if got := e.modelTier(tc.model); got != tc.want {
			t.Errorf("modelTier(%q) = %s, want %s", tc.model, got, tc.want)
		}
	}
}

func TestModelTier_WithModelMap(t *testing.T) {
	cfg := newTestConfig()
	cfg.ModelMap = map[string]map[string]string{
		"big-model": {
			"nvidia": "nemotron-ultra",
		},
	}
	e := New(cfg)
	// Should resolve "big-model" -> "nemotron-ultra" and find premium
	if got := e.modelTier("big-model"); got != "premium" {
		t.Fatalf("expected premium for resolved model, got %s", got)
	}
}

func TestDecide_Reason(t *testing.T) {
	e := New(newTestConfig())
	c := &classify.Result{TaskType: "conversation", Complexity: "moderate", ContextType: "new_session"}
	dec := e.Decide(c, nil, "default", "")
	if dec.Reason != "tier_mid_moderate" {
		t.Fatalf("expected 'tier_mid_moderate', got %s", dec.Reason)
	}
}

func TestDecide_StickyReason(t *testing.T) {
	e := New(newTestConfig())
	s := healthySession()
	c := &classify.Result{TaskType: "coding", Complexity: "moderate", ContextType: "session_continuation"}
	dec := e.Decide(c, s, "default", "")
	if dec.Reason != "session_sticky_cache_optimal" {
		t.Fatalf("expected sticky reason, got %s", dec.Reason)
	}
}

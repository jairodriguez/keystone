package router

import (
	"testing"

	"github.com/clawdbot/keystone/internal/config"
	"github.com/clawdbot/keystone/internal/keypool"
	"github.com/clawdbot/keystone/internal/provider"
	"github.com/clawdbot/keystone/internal/registry"
)

func newTestConfig() *config.Config {
	return &config.Config{
		Tiers: map[string]config.TierConfig{
			"premium": {Models: []string{"glm-5.1", "nemotron-ultra"}},
			"coder":   {Models: []string{"qwen3-coder"}},
			"mid":     {Models: []string{"nemotron-super"}},
			"free":    {Models: []string{"gemma-4-31b", "llama-70b"}},
		},
		Fallback: config.FallbackConfig{
			Chains: map[string][]string{
				"premium": {"zai", "nvidia", "zen", "openrouter"},
				"mid":     {"nvidia", "openrouter"},
				"free":    {"nvidia", "openrouter"},
			},
			CrossTier: true,
		},
	}
}

func newTestRegistry(t *testing.T) *provider.Registry {
	t.Helper()
	reg := provider.NewRegistry()

	zai := provider.New("zai", "https://api.z.ai", keypool.NewPool("zai", []string{"z-key-1"}), []string{"glm-5.1"})
	reg.Register(zai)

	nvidia := provider.New("nvidia", "https://ai.api.nvidia.com", keypool.NewPool("nvidia", []string{"nv-key-1", "nv-key-2"}), []string{"nemotron-ultra", "nemotron-super", "qwen3-coder", "gemma-4-31b", "llama-70b", "glm-5.1"})
	reg.Register(nvidia)

	zen := provider.New("zen", "https://opencode.ai/zen", keypool.NewPool("zen", []string{"zen-key-1"}), []string{"glm-5.1"})
	reg.Register(zen)

	openrouter := provider.New("openrouter", "https://openrouter.ai/api", keypool.NewPool("openrouter", []string{"or-key-1"}), []string{"nemotron-super", "gemma-4-31b"})
	reg.Register(openrouter)

	return reg
}

func newTestModelReg() *registry.ModelRegistry {
	return &registry.ModelRegistry{
		Models: []registry.ModelConfig{
			{ID: "glm-5.1", ContextWindow: 65536},
			{ID: "nemotron-ultra", ContextWindow: 1000000},
			{ID: "nemotron-super", ContextWindow: 1000000},
			{ID: "qwen3-coder", ContextWindow: 1000000},
			{ID: "gemma-4-31b", ContextWindow: 1000000},
			{ID: "llama-70b", ContextWindow: 131000},
		},
	}
}

func TestNextLowerTier(t *testing.T) {
	cases := []struct{ tier, want string }{
		{"premium", "coder"},
		{"coder", "mid"},
		{"mid", "free"},
		{"free", ""},
		{"unknown", ""},
	}
	for _, tc := range cases {
		if got := NextLowerTier(tc.tier); got != tc.want {
			t.Errorf("NextLowerTier(%q) = %q, want %q", tc.tier, got, tc.want)
		}
	}
}

func TestNextHigherTier(t *testing.T) {
	cases := []struct{ tier, want string }{
		{"free", "mid"},
		{"mid", "coder"},
		{"coder", "premium"},
		{"premium", ""},
		{"unknown", ""},
	}
	for _, tc := range cases {
		if got := NextHigherTier(tc.tier); got != tc.want {
			t.Errorf("NextHigherTier(%q) = %q, want %q", tc.tier, got, tc.want)
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
		if got := TierRank(tc.tier); got != tc.rank {
			t.Errorf("TierRank(%q) = %d, want %d", tc.tier, got, tc.rank)
		}
	}
}

func TestSelectProviderAndKey_SelectsFromChain(t *testing.T) {
	r := New(newTestRegistry(t), newTestConfig(), nil)
	dec, err := r.SelectProviderAndKey("premium", "glm-5.1", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.Provider.Name != "zai" {
		t.Fatalf("expected zai, got %s", dec.Provider.Name)
	}
	if dec.Model != "glm-5.1" {
		t.Fatalf("expected model glm-5.1, got %s", dec.Model)
	}
	if dec.Tier != "premium" {
		t.Fatalf("expected tier premium, got %s", dec.Tier)
	}
	if dec.Key == nil {
		t.Fatal("expected key to be set")
	}
}

func TestSelectProviderAndKey_SkipsUnhealthyProvider(t *testing.T) {
	reg := newTestRegistry(t)
	cfg := newTestConfig()

	// Kill zai's only key
	zaiProvider, _ := reg.Get("zai")
	zk, _ := zaiProvider.Pool.Select()
	zaiProvider.Pool.TriggerCooldown(zk.ID, 403, 0)

	r := New(reg, cfg, nil)
	dec, err := r.SelectProviderAndKey("premium", "glm-5.1", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should fall through zai to nvidia
	if dec.Provider.Name != "nvidia" {
		t.Fatalf("expected nvidia fallback, got %s", dec.Provider.Name)
	}
}

func TestSelectProviderAndKey_FallsBackToTierDefaults(t *testing.T) {
	reg := newTestRegistry(t)
	r := New(reg, newTestConfig(), nil)
	// Requesting a model not in the free tier should fall back to tier defaults (gemma-4-31b, llama-70b)
	dec, err := r.SelectProviderAndKey("free", "nonexistent-model", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.Tier != "free" {
		t.Fatalf("expected free tier, got %s", dec.Tier)
	}
}

func TestSelectProviderAndKey_RespectsContextWindow(t *testing.T) {
	reg := newTestRegistry(t)
	cfg := newTestConfig()
	modelReg := newTestModelReg()
	r := New(reg, cfg, modelReg)

	// glm-5.1 has 65536 context window, requesting with 70000 should skip it
	// premium chain: zai (glm-5.1) -> nvidia (glm-5.1, nemotron-ultra)
	// glm-5.1 should be skipped by context check, so nemotron-ultra on nvidia should be chosen
	dec, err := r.SelectProviderAndKey("premium", "glm-5.1", 70000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.Provider.Name != "nvidia" {
		t.Fatalf("expected nvidia fallback for context, got %s", dec.Provider.Name)
	}
	if dec.Model != "nemotron-ultra" {
		t.Fatalf("expected nemotron-ultra (1M ctx), got %s", dec.Model)
	}
}

func TestSelectProviderAndKey_CrossTierFallback(t *testing.T) {
	// Custom setup: premium can only use zai, but zai is dead.
	// Coder can use nvidia with the same model.
	zaiP := keypool.NewPool("zai", []string{"z-key-1"})
	zk, _ := zaiP.Select()
	zaiP.TriggerCooldown(zk.ID, 403, 0) // zai dead

	nvP := keypool.NewPool("nvidia", []string{"nv-key-1"})
	reg := provider.NewRegistry()
	reg.Register(provider.New("zai", "https://z.ai", zaiP, []string{"qwen3-coder"}))
	reg.Register(provider.New("nvidia", "https://nv.ai", nvP, []string{"qwen3-coder"}))

	cfg := &config.Config{
		Tiers: map[string]config.TierConfig{
			"premium": {Models: []string{"qwen3-coder"}},
			"coder":   {Models: []string{"qwen3-coder"}},
		},
		Fallback: config.FallbackConfig{
			Chains: map[string][]string{
				"premium": {"zai"},
			},
			CrossTier: true,
		},
	}

	r := New(reg, cfg, nil)
	dec, err := r.SelectProviderAndKey("premium", "qwen3-coder", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.Tier != "coder" {
		t.Fatalf("expected cross-tier fallback to coder, got %s", dec.Tier)
	}
	if dec.Provider.Name != "nvidia" {
		t.Fatalf("expected nvidia, got %s", dec.Provider.Name)
	}
}

func killPool(reg *provider.Registry, name string) {
	p, ok := reg.Get(name)
	if !ok {
		return
	}
	for _, k := range p.Pool.AllKeys() {
		p.Pool.TriggerCooldown(k.ID, 403, 0)
	}
}

func TestSelectProviderAndKey_CrossTierFallbackDisabled(t *testing.T) {
	reg := newTestRegistry(t)
	cfg := newTestConfig()
	cfg.Fallback.CrossTier = false

	// Kill all providers in premium chain
	killAllKeys(reg)

	r := New(reg, cfg, nil)
	_, err := r.SelectProviderAndKey("premium", "glm-5.1", 0)
	if err == nil {
		t.Fatal("expected error when all providers dead and cross-tier disabled")
	}
}

func killAllKeys(reg *provider.Registry) {
	for _, provName := range []string{"zai", "nvidia", "zen", "openrouter"} {
		prov, ok := reg.Get(provName)
		if !ok {
			continue
		}
		for _, k := range prov.Pool.AllKeys() {
			prov.Pool.TriggerCooldown(k.ID, 403, 0)
		}
	}
}

func TestSelectProviderAndKey_NoFallbackChainUsesOnModelProviders(t *testing.T) {
	cfg := newTestConfig()
	delete(cfg.Fallback.Chains, "free")

	reg := provider.NewRegistry()
	nv := provider.New("nvidia", "https://ai.api.nvidia.com", keypool.NewPool("nvidia", []string{"nv-key-1"}), []string{"gemma-4-31b", "llama-70b"})
	reg.Register(nv)
	or := provider.New("openrouter", "https://openrouter.ai", keypool.NewPool("openrouter", []string{"or-key-1"}), []string{"gemma-4-31b"})
	reg.Register(or)

	r := New(reg, cfg, nil)
	dec, err := r.SelectProviderAndKey("free", "gemma-4-31b", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.Provider.Name != "nvidia" {
		t.Fatalf("expected nvidia, got %s", dec.Provider.Name)
	}
}

func TestSelectForTier(t *testing.T) {
	r := New(newTestRegistry(t), newTestConfig(), nil)
	dec, err := r.SelectForTier("premium")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.Tier != "premium" {
		t.Fatalf("expected tier premium, got %s", dec.Tier)
	}
}

func TestSelectForTier_UnknownTier(t *testing.T) {
	r := New(newTestRegistry(t), newTestConfig(), nil)
	_, err := r.SelectForTier("nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown tier")
	}
}

func TestDecisionString(t *testing.T) {
	k := &keypool.Key{ID: "test-key"}
	prov := provider.New("test-prov", "https://example.com", keypool.NewPool("test", []string{"k"}), []string{"m1"})
	d := &Decision{
		Tier:     "premium",
		Provider: prov,
		Key:      k,
		Model:    "m1",
		Sticky:   true,
		Reason:   "selected_test-prov",
	}
	s := d.String()
	if len(s) < 30 {
		t.Fatalf("expected informative string, got %q", s)
	}
}

func TestDecisionString_NilProvider(t *testing.T) {
	d := &Decision{Tier: "free", Model: "m1", Sticky: false, Reason: "fallback"}
	s := d.String()
	if len(s) < 10 {
		t.Fatalf("expected string, got %q", s)
	}
}

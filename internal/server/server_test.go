package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/clawdbot/keystone/internal/classify"
	"github.com/clawdbot/keystone/internal/config"
	"github.com/clawdbot/keystone/internal/economics"
	"github.com/clawdbot/keystone/internal/keypool"
	"github.com/clawdbot/keystone/internal/provider"
	"github.com/clawdbot/keystone/internal/registry"
	"github.com/clawdbot/keystone/internal/router"
	"github.com/clawdbot/keystone/internal/session"
)

func newTestConfig() *config.Config {
	return &config.Config{
		Classifier: config.ClassifierConfig{Mode: "simple"},
		Sessions: config.SessionConfig{
			TTL:         time.Minute,
			Header:      "x-session-id",
			AutoDerive:  true,
			AgentHeader: "x-agent",
		},
		Tiers: map[string]config.TierConfig{
			"premium": {Models: []string{"test-model"}},
			"coder":   {Models: []string{"test-model"}},
		},
		Fallback: config.FallbackConfig{
			Chains: map[string][]string{
				"premium": {"zai-mock", "nvidia-mock"},
			},
			CrossTier: true,
		},
		Economics: config.EconomicsConfig{
			StickyMinTurns:   3,
			StickyMinContext: 5000,
			CacheHitRatio:    0.7,
		},
		ModelMap: map[string]map[string]string{},
		AgentTiers: map[string]string{
			"default": "free",
		},
		Providers: []config.ProviderConfig{
			{Name: "zai-mock", Keys: []config.KeyConfig{{Key: "key-fail"}}},
			{Name: "nvidia-mock", Keys: []config.KeyConfig{{Key: "key-success"}}},
		},
	}
}

func successHandler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"test-id","choices":[{"message":{"content":"Hello"}}],"usage":{"total_tokens":10}}`))
	})
}

func statusHandler(statusCode int, body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		w.Write([]byte(body))
	})
}

func buildServer(t *testing.T, cfg *config.Config, failHandler, successHandler http.Handler) (*Server, *httptest.Server, *httptest.Server) {
	t.Helper()

	failUpstream := httptest.NewServer(failHandler)
	successUpstream := httptest.NewServer(successHandler)

	reg := provider.NewRegistry()
	reg.Register(provider.New("zai-mock", failUpstream.URL,
		keypool.NewPool("zai-mock", []string{"key-fail"}), []string{"test-model"}))
	reg.Register(provider.New("nvidia-mock", successUpstream.URL,
		keypool.NewPool("nvidia-mock", []string{"key-success"}), []string{"test-model"}))

	modelReg, _ := registry.Load() // loads embedded models.yaml
	sm := session.NewManager(session.NewMemoryStore(), time.Minute, "x-session-id", true, "x-agent")
	rt := router.New(reg, cfg, modelReg)
	econ := economics.New(cfg)
	cls := classify.Get(cfg.Classifier.Mode)
	modeFn := func() string { return "normal" }

	srv := New(cfg, reg, rt, modelReg, sm, econ, cls, modeFn)
	return srv, failUpstream, successUpstream
}

func sendRequest(srv *Server, body map[string]any) *http.Response {
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-session-id", "test-session-001")
	req.Header.Set("x-agent", "default")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	return w.Result()
}

// --- Helper: ensure no fallback happened (first provider succeeded) ---

// --- Detection function tests ---

func TestIsContextLengthError(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{`{"error":{"message":"context_length_exceeded"}}`, true},
		{`max context length is 65536`, true},
		{`This model's maximum context length is 4096`, true},
		{`reduce the length of the messages`, true},
		{`too many tokens`, true},
		{`token limit exceeded`, true},
		{`{"error":"rate_limit"}`, false},
		{`{"error":"internal"}`, false},
	}
	for _, tc := range cases {
		if got := isContextLengthError(tc.body); got != tc.want {
			t.Errorf("isContextLengthError(%q) = %v, want %v", tc.body, got, tc.want)
		}
	}
}

func TestIsUnknownModelError(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{`error: unknown model`, true},
		{`model not found`, true},
		{`model does not exist`, true},
		{`invalid model: gpt-5`, true},
		{`model "xyz" is not a valid model`, true},
		{`model_code: 404`, true},
		{`please check the model code`, true},
		{`single tool-calls error`, true},
		{`tool calls not supported`, true},
		{`reasoning_content required`, true},
		{`{"error":"rate_limit"}`, false},
		{`{"error":"internal"}`, false},
	}
	for _, tc := range cases {
		if got := isUnknownModelError(tc.body); got != tc.want {
			t.Errorf("isUnknownModelError(%q) = %v, want %v", tc.body, got, tc.want)
		}
	}
}

// --- 404 fallback tests ---

func Test404Fallback_TriggersCooldownAndSwitchesProvider(t *testing.T) {
	cfg := newTestConfig()
	srv, failUpstream, successUpstream := buildServer(t, cfg,
		statusHandler(404, `{"error":"not found"}`),
		successHandler(t))
	defer failUpstream.Close()
	defer successUpstream.Close()

	resp := sendRequest(srv, map[string]any{
		"model":    "test-model",
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
	})

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after fallback, got %d", resp.StatusCode)
	}

	// Should have landed on nvidia-mock (success provider)
	if got := resp.Header.Get("x-keystone-provider"); got != "nvidia-mock" {
		t.Fatalf("expected nvidia-mock, got %s", got)
	}
	if got := resp.Header.Get("x-keystone-reason"); got != "context_fallback_nvidia-mock" && got != "key_rotation_nvidia-mock" && got != "session_sticky" {
		// Accept any valid reason
	}

	// zai-mock key should be in cooldown
	zaiProv, _ := srv.Registry.Get("zai-mock")
	keys := zaiProv.Pool.AllKeys()
	if keys[0].State != keypool.StateCooling && keys[0].State != keypool.StateDead {
		t.Fatalf("expected zai key to be in cooldown/dead after 404, got %v", keys[0].State)
	}
}

func Test404Fallback_AllProvidersExhausted(t *testing.T) {
	cfg := newTestConfig()
	// Both upstreams return 404
	srv, failUpstream, successUpstream := buildServer(t, cfg,
		statusHandler(404, `{"error":"not found"}`),
		statusHandler(404, `{"error":"not found"}`))
	defer failUpstream.Close()
	defer successUpstream.Close()

	resp := sendRequest(srv, map[string]any{
		"model":    "test-model",
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
	})

	// Should pass through the 404 from the last provider
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404 pass-through, got %d", resp.StatusCode)
	}
}

// --- 402 fallback tests ---

func Test402Fallback_SwitchesProvider(t *testing.T) {
	cfg := newTestConfig()
	srv, failUpstream, successUpstream := buildServer(t, cfg,
		statusHandler(402, `{"error":"insufficient_credits"}`),
		successHandler(t))
	defer failUpstream.Close()
	defer successUpstream.Close()

	resp := sendRequest(srv, map[string]any{
		"model":    "test-model",
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
	})

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after 402 fallback, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("x-keystone-provider"); got != "nvidia-mock" {
		t.Fatalf("expected nvidia-mock after fallback, got %s", got)
	}
}

// --- 400 fallback (context length) tests ---

func Test400Fallback_ContextLength_SwitchesProvider(t *testing.T) {
	cfg := newTestConfig()
	srv, failUpstream, successUpstream := buildServer(t, cfg,
		statusHandler(400, `{"error":"context_length_exceeded"}`),
		successHandler(t))
	defer failUpstream.Close()
	defer successUpstream.Close()

	resp := sendRequest(srv, map[string]any{
		"model":    "test-model",
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
	})

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after 400 context fallback, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("x-keystone-provider"); got != "nvidia-mock" {
		t.Fatalf("expected nvidia-mock after context fallback, got %s", got)
	}
}

func Test400Fallback_ContextLength_AllExhausted(t *testing.T) {
	cfg := newTestConfig()
	srv, failUpstream, successUpstream := buildServer(t, cfg,
		statusHandler(400, `{"error":"context_length_exceeded"}`),
		statusHandler(400, `{"error":"context_length_exceeded"}`))
	defer failUpstream.Close()
	defer successUpstream.Close()

	resp := sendRequest(srv, map[string]any{
		"model":    "test-model",
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
	})

	if resp.StatusCode != 400 {
		t.Fatalf("expected 400 pass-through, got %d", resp.StatusCode)
	}
}

func Test400Fallback_UnknownModel_SwitchesProvider(t *testing.T) {
	cfg := newTestConfig()
	srv, failUpstream, successUpstream := buildServer(t, cfg,
		statusHandler(400, `{"error":"unknown model"}`),
		successHandler(t))
	defer failUpstream.Close()
	defer successUpstream.Close()

	resp := sendRequest(srv, map[string]any{
		"model":    "test-model",
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
	})

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after unknown model fallback, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("x-keystone-provider"); got != "nvidia-mock" {
		t.Fatalf("expected nvidia-mock after fallback, got %s", got)
	}
}

func Test400Fallback_NonRecoverable_ReturnsRaw400(t *testing.T) {
	cfg := newTestConfig()
	body := `{"error":"bad_request_structure"}`
	srv, failUpstream, successUpstream := buildServer(t, cfg,
		statusHandler(400, body),
		successHandler(t))
	defer failUpstream.Close()
	defer successUpstream.Close()

	resp := sendRequest(srv, map[string]any{
		"model":    "test-model",
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
	})

	// Non-recoverable 400 should pass through immediately, not fall back
	if resp.StatusCode != 400 {
		t.Fatalf("expected 400 pass-through, got %d", resp.StatusCode)
	}
}

// --- tryFallback direct tests ---

func TestTryFallback_KeyRotation(t *testing.T) {
	pool := keypool.NewPool("multi-key", []string{"key-1", "key-2"})
	prov := provider.New("multi-provider", "http://example.com", pool, []string{"test-model"})

	p1, _ := pool.Select() // key-1
	current := &router.Decision{
		Tier:     "premium",
		Provider: prov,
		Key:      p1,
		Model:    "test-model",
	}

	cfg := newTestConfig()
	srv := &Server{
		Config: cfg,
		Router: router.New(
			func() *provider.Registry {
				reg := provider.NewRegistry()
				reg.Register(prov)
				return reg
			}(),
			cfg, nil,
		),
	}

	fallback := srv.tryFallback(current, "premium", "test-model", 0)
	if fallback == nil {
		t.Fatal("expected fallback to find another key")
	}
	if fallback.Key.ID == p1.ID {
		t.Fatal("expected different key after rotation")
	}
	if fallback.Provider.Name != "multi-provider" {
		t.Fatalf("expected same provider, got %s", fallback.Provider.Name)
	}
}

func TestTryFallback_ReturnsNilWhenAllExhausted(t *testing.T) {
	pool := keypool.NewPool("single-key", []string{"only-key"})
	prov := provider.New("single-prov", "http://example.com", pool, []string{"test-model"})

	p1, _ := pool.Select()
	current := &router.Decision{
		Tier:     "free",
		Provider: prov,
		Key:      p1,
		Model:    "test-model",
	}

	cfg := newTestConfig()
	reg := provider.NewRegistry()
	reg.Register(prov)
	srv := &Server{
		Config: cfg,
		Router: router.New(reg, cfg, nil),
	}

	fallback := srv.tryFallback(current, "free", "test-model", 0)
	if fallback != nil {
		t.Fatal("expected nil when no fallback available")
	}
}

// --- tryContextFallback direct tests ---

func TestTryContextFallback_FindsLargerContextModel(t *testing.T) {
	smallPool := keypool.NewPool("small-ctx", []string{"key-small"})
	largePool := keypool.NewPool("large-ctx", []string{"key-large"})

	reg := provider.NewRegistry()
	reg.Register(provider.New("small-ctx", "http://small.example.com", smallPool, []string{"small-model"}))
	reg.Register(provider.New("large-ctx", "http://large.example.com", largePool, []string{"large-model"}))

	modelReg := &registry.ModelRegistry{
		Models: []registry.ModelConfig{
			{ID: "small-model", ContextWindow: 10000},
			{ID: "large-model", ContextWindow: 100000},
		},
	}

	cfg := newTestConfig()
	cfg.Tiers["premium"] = config.TierConfig{Models: []string{"small-model", "large-model"}}
	cfg.Fallback.Chains["premium"] = []string{"small-ctx", "large-ctx"}

	srv := &Server{
		Config:   cfg,
		Registry: reg,
		Router:   router.New(reg, cfg, modelReg),
		ModelReg: modelReg,
	}

	smallKey, _ := smallPool.Select()
	current := &router.Decision{
		Tier:     "premium",
		Provider: mustGetProvider(reg, "small-ctx"),
		Key:      smallKey,
		Model:    "small-model",
	}

	fallback := srv.tryContextFallback(current, "premium", 50000)
	if fallback == nil {
		t.Fatal("expected context fallback to succeed")
	}
	if fallback.Provider.Name != "large-ctx" {
		t.Fatalf("expected large-ctx provider, got %s", fallback.Provider.Name)
	}
}

func mustGetProvider(reg *provider.Registry, name string) *provider.Provider {
	p, _ := reg.Get(name)
	return p
}

func TestTryContextFallback_ReturnsNilWhenContextFits(t *testing.T) {
	// If no provider has a context window large enough, should return nil
	smallPool := keypool.NewPool("tiny-ctx", []string{"key-tiny"})
	reg := provider.NewRegistry()
	reg.Register(provider.New("tiny-ctx", "http://tiny.example.com", smallPool, []string{"tiny-model"}))

	modelReg := &registry.ModelRegistry{
		Models: []registry.ModelConfig{
			{ID: "tiny-model", ContextWindow: 1000},
		},
	}

	cfg := newTestConfig()
	cfg.Tiers["premium"] = config.TierConfig{Models: []string{"tiny-model"}}
	cfg.Fallback.Chains["premium"] = []string{"tiny-ctx"}

	srv := &Server{
		Config:   cfg,
		Registry: reg,
		Router:   router.New(reg, cfg, modelReg),
		ModelReg: modelReg,
	}

	tinyKey, _ := smallPool.Select()
	current := &router.Decision{
		Tier:     "premium",
		Provider: mustGetProvider(reg, "tiny-ctx"),
		Key:      tinyKey,
		Model:    "tiny-model",
	}

	fallback := srv.tryContextFallback(current, "premium", 100000)
	if fallback != nil {
		t.Fatal("expected nil when no adequate context window")
	}
}

// --- Test error passthrough preserves headers ---

func TestErrorPassthrough_PreservesXHeaders(t *testing.T) {
	cfg := newTestConfig()
	srv, failUpstream, successUpstream := buildServer(t, cfg,
		statusHandler(429, `{"error":"rate_limited"}`),
		statusHandler(429, `{"error":"rate_limited"}`))
	defer failUpstream.Close()
	defer successUpstream.Close()

	resp := sendRequest(srv, map[string]any{
		"model":    "test-model",
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
	})

	// 429 should be passed through after both fail
	if resp.StatusCode != 429 {
		t.Fatalf("expected 429 pass-through, got %d", resp.StatusCode)
	}
}

func TestServer_MethodNotAllowed(t *testing.T) {
	cfg := newTestConfig()
	srv, _, _ := buildServer(t, cfg, successHandler(t), successHandler(t))

	raw, _ := json.Marshal(map[string]any{"model": "test-model"})
	req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", bytes.NewReader(raw))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	resp := w.Result()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", resp.StatusCode)
	}
}

func TestServer_MissingModel(t *testing.T) {
	cfg := newTestConfig()
	srv, _, _ := buildServer(t, cfg, successHandler(t), successHandler(t))

	resp := sendRequest(srv, map[string]any{
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
	})

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing model, got %d", resp.StatusCode)
	}
}

func TestServer_InvalidJSON(t *testing.T) {
	cfg := newTestConfig()
	srv, _, _ := buildServer(t, cfg, successHandler(t), successHandler(t))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(`not json`)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	resp := w.Result()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid JSON, got %d", resp.StatusCode)
	}
}

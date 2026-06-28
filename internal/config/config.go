package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server    ServerConfig    `yaml:"server"`
	Sessions  SessionConfig   `yaml:"sessions"`
	ZenProxy  ZenProxyConfig  `yaml:"zen_proxy"`
	Classifier ClassifierConfig `yaml:"classifier"`
	Providers []ProviderConfig `yaml:"providers"`
	Tiers     map[string]TierConfig `yaml:"tiers"`
	Fallback  FallbackConfig  `yaml:"fallback"`
	ModelMap  map[string]map[string]string `yaml:"model_map"`
	AgentTiers map[string]string `yaml:"agent_tiers"`
	Economics EconomicsConfig `yaml:"economics"`
	Metrics   MetricsConfig   `yaml:"metrics"`
	API       APIConfig       `yaml:"api"`
	Redis     RedisConfig     `yaml:"redis"`
}

type ZenProxyConfig struct {
	Enabled bool   `yaml:"enabled"`
	Listen  string `yaml:"listen"`
}

type ServerConfig struct {
	Listen       string        `yaml:"listen"`
	ReadTimeout  time.Duration `yaml:"read_timeout"`
	WriteTimeout time.Duration `yaml:"write_timeout"`
	IdleTimeout  time.Duration `yaml:"idle_timeout"`
	ProxyTimeout time.Duration `yaml:"proxy_timeout"`
}

type SessionConfig struct {
	TTL       time.Duration `yaml:"ttl"`
	Header    string        `yaml:"header"`
	AutoDerive bool         `yaml:"auto_derive"`
	AgentHeader string      `yaml:"agent_header"`
}

type ClassifierConfig struct {
	Mode   string         `yaml:"mode"`
	Gemini GeminiConfig   `yaml:"gemini"`
}

type GeminiConfig struct {
	APIKey  string `yaml:"api_key"`
	BaseURL string `yaml:"base_url"`
	Model   string `yaml:"model"`
}

type ProviderConfig struct {
	Name     string         `yaml:"name"`
	BaseURL  string         `yaml:"base_url"`
	Keys     []KeyConfig    `yaml:"keys"`
	Models   []string       `yaml:"models"`
	Cooldown CooldownConfig `yaml:"cooldown"`
	RateLimit RateLimitConfig `yaml:"rate_limit"`
	MaxConcurrent int       `yaml:"max_concurrent"`
}

type KeyConfig struct {
	Key    string `yaml:"key"`
	Weight int    `yaml:"weight"`
}

type CooldownConfig struct {
	R429 string `yaml:"r429"`
	R500 string `yaml:"r500"`
	R401 string `yaml:"r401"`
}

type RateLimitConfig struct {
	RequestsPerMinute int `yaml:"requests_per_minute"`
}

type TierConfig struct {
	Models []string `yaml:"models"`
}

type FallbackConfig struct {
	Chains   map[string][]string `yaml:"chains"`
	CrossTier bool               `yaml:"cross_tier"`
}

type EconomicsConfig struct {
	StickyMinTurns   int     `yaml:"sticky_min_turns"`
	StickyMinContext int     `yaml:"sticky_min_context"`
	CacheHitRatio    float64 `yaml:"cache_hit_ratio"`
}

type MetricsConfig struct {
	Prometheus bool   `yaml:"prometheus"`
	Path       string `yaml:"path"`
}

type APIConfig struct {
	Enabled  bool   `yaml:"enabled"`
	BasePath string `yaml:"base_path"`
}

type RedisConfig struct {
	URL string `yaml:"url"`
}

func Default() *Config {
	return &Config{
		Server: ServerConfig{
			Listen:       ":8080",
			ReadTimeout:  30 * time.Second,
			WriteTimeout: 300 * time.Second,
			IdleTimeout:  120 * time.Second,
			ProxyTimeout: 120 * time.Second,
		},
		Sessions: SessionConfig{
			TTL:        30 * time.Minute,
			Header:     "x-session-id",
			AutoDerive: true,
			AgentHeader: "x-agent",
		},
		Classifier: ClassifierConfig{
			Mode: "normal",
			Gemini: GeminiConfig{
				BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai",
				Model:   "gemini-2.5-flash",
			},
		},
		Economics: EconomicsConfig{
			StickyMinTurns:   3,
			StickyMinContext: 50000,
			CacheHitRatio:    0.7,
		},
		Metrics: MetricsConfig{
			Prometheus: true,
			Path:       "/metrics",
		},
		API: APIConfig{
			Enabled:  true,
			BasePath: "/api",
		},
	}
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	cfg := Default()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	cfg.expandEnvRefs()

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}

	return cfg, nil
}

func (c *Config) expandEnvRefs() {
	for i := range c.Providers {
		for j := range c.Providers[i].Keys {
			c.Providers[i].Keys[j].Key = expandEnv(c.Providers[i].Keys[j].Key)
		}
	}
	c.Classifier.Gemini.APIKey = expandEnv(c.Classifier.Gemini.APIKey)
	c.Redis.URL = expandEnv(c.Redis.URL)
}

func expandEnv(val string) string {
	if len(val) > 3 && val[0] == '$' && val[1] == '{' {
		envName := val[2 : len(val)-1]
		return os.Getenv(envName)
	}
	return val
}

func (c *Config) Validate() error {
	if len(c.Providers) == 0 {
		return fmt.Errorf("at least one provider required")
	}
	for _, p := range c.Providers {
		if p.Name == "" {
			return fmt.Errorf("provider missing name")
		}
		if p.BaseURL == "" {
			return fmt.Errorf("provider %s missing base_url", p.Name)
		}
		if len(p.Keys) == 0 {
			return fmt.Errorf("provider %s has no keys", p.Name)
		}
	}
	if c.Sessions.Header == "" {
		c.Sessions.Header = "x-session-id"
	}
	return nil
}

func (c *Config) FindProvider(name string) *ProviderConfig {
	for i := range c.Providers {
		if c.Providers[i].Name == name {
			return &c.Providers[i]
		}
	}
	return nil
}

func (c *Config) ParseCooldown(provider string, code string) time.Duration {
	p := c.FindProvider(provider)
	if p == nil {
		return 60 * time.Second
	}
	var raw string
	switch code {
	case "r429":
		raw = p.Cooldown.R429
	case "r500":
		raw = p.Cooldown.R500
	case "r401":
		raw = p.Cooldown.R401
	default:
		return 60 * time.Second
	}
	if raw == "permanent" || raw == "" {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 60 * time.Second
	}
	return d
}

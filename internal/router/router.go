package router

import (
	"fmt"

	"github.com/clawdbot/keystone/internal/config"
	"github.com/clawdbot/keystone/internal/keypool"
	"github.com/clawdbot/keystone/internal/provider"
)

type Decision struct {
	Tier     string
	Provider *provider.Provider
	Key      *keypool.Key
	Model    string
	Sticky   bool
	Reason   string
}

func (d *Decision) String() string {
	provName := "?"
	if d.Provider != nil {
		provName = d.Provider.Name
	}
	keyID := "?"
	if d.Key != nil {
		keyID = d.Key.ID
	}
	return fmt.Sprintf("tier=%s provider=%s key=%s model=%s sticky=%v reason=%s",
		d.Tier, provName, keyID, d.Model, d.Sticky, d.Reason)
}

type Router struct {
	Registry  *provider.Registry
	Config    *config.Config
}

func New(reg *provider.Registry, cfg *config.Config) *Router {
	return &Router{Registry: reg, Config: cfg}
}

func (r *Router) SelectProviderAndKey(tier, model string) (*Decision, error) {
	chain, ok := r.Config.Fallback.Chains[tier]
	if !ok || len(chain) == 0 {
		providers := r.Registry.FindProvidersForModel(model)
		if len(providers) == 0 {
			return nil, fmt.Errorf("no providers for model %s in tier %s", model, tier)
		}
		chain = make([]string, 0, len(providers))
		for _, p := range providers {
			chain = append(chain, p.Name)
		}
	}

	for _, provName := range chain {
		p, ok := r.Registry.Get(provName)
		if !ok {
			continue
		}
		if !p.HasModel(model) {
			continue
		}
		if !p.Pool.IsHealthy() {
			continue
		}
		key, err := p.Pool.Select()
		if err != nil {
			continue
		}
		modelName := provider.ResolveModelName(model, p.Name, r.Config.ModelMap)
		return &Decision{
			Tier:     tier,
			Provider: p,
			Key:      key,
			Model:    modelName,
			Sticky:   false,
			Reason:   "selected_" + p.Name,
		}, nil
	}

	if r.Config.Fallback.CrossTier {
		lower := NextLowerTier(tier)
		if lower != "" {
			return r.SelectProviderAndKey(lower, model)
		}
	}

	return nil, fmt.Errorf("all providers exhausted for tier %s model %s", tier, model)
}

func (r *Router) SelectForTier(tier string) (*Decision, error) {
	tierCfg, ok := r.Config.Tiers[tier]
	if !ok || len(tierCfg.Models) == 0 {
		return r.SelectProviderAndKey(tier, "")
	}
	return r.SelectProviderAndKey(tier, tierCfg.Models[0])
}

func NextLowerTier(tier string) string {
	switch tier {
	case "premium":
		return "coder"
	case "coder":
		return "mid"
	case "mid":
		return "free"
	default:
		return ""
	}
}

func TierRank(tier string) int {
	switch tier {
	case "free":
		return 0
	case "mid":
		return 1
	case "coder":
		return 2
	case "premium":
		return 3
	default:
		return 1
	}
}

var ErrNoProviders = fmt.Errorf("no providers available")

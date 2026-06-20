package economics

import (
	"fmt"

	"github.com/clawdbot/keystone/internal/classify"
	"github.com/clawdbot/keystone/internal/config"
	"github.com/clawdbot/keystone/internal/session"
)

type Engine struct {
	Config *config.Config
}

func New(cfg *config.Config) *Engine {
	return &Engine{Config: cfg}
}

type Decision struct {
	Tier       string
	Sticky     bool
	Reason     string
	Downgrade  bool
}

func (e *Engine) Decide(c *classify.Result, s *session.Session, agent string, requestedModel string) *Decision {
	targetTier := e.determineTier(c, agent, requestedModel)
	sticky := e.shouldStaySticky(c, s, targetTier)

	reason := fmt.Sprintf("tier_%s_%s", targetTier, c.Complexity)

	if sticky {
		reason = "session_sticky_cache_optimal"
	}

	downgrade := false
	if s != nil && s.Tier != "" && tierRank(targetTier) < tierRank(s.Tier) {
		downgrade = true
	}

	return &Decision{
		Tier:      targetTier,
		Sticky:    sticky,
		Reason:    reason,
		Downgrade: downgrade,
	}
}

func (e *Engine) determineTier(c *classify.Result, agent string, requestedModel string) string {
	modelTier := ""
	if requestedModel != "" {
		modelTier = e.modelTier(requestedModel)
	}

	if agentTier, ok := e.Config.AgentTiers[agent]; ok {
		tier := e.determineComplexityTier(c)
		// Use the highest of: agent floor, model tier, complexity tier
		rank := tierRank(tier)
		if tierRank(agentTier) > rank {
			rank = tierRank(agentTier)
		}
		if tierRank(modelTier) > rank {
			rank = tierRank(modelTier)
		}
		return tierFromRank(rank)
	}

	// No agent tier floor: use max of model tier and complexity tier
	complexityTier := e.determineComplexityTier(c)
	if tierRank(modelTier) > tierRank(complexityTier) {
		return modelTier
	}
	return complexityTier
}

func (e *Engine) modelTier(model string) string {
	// First check model_map to resolve the actual provider model name
	resolvedModel := model
	if e.Config.ModelMap != nil {
		if mapping, ok := e.Config.ModelMap[model]; ok {
			for _, m := range mapping {
				if m != "" {
					resolvedModel = m
					break
				}
			}
		}
	}
	
	// Check tier configs ONLY (not fallback chains) for model -> tier mapping
	// Return the highest tier that contains this model
	var highestTier string
	highestRank := -1
	for _, m := range []string{model, resolvedModel} {
		for tier, tierCfg := range e.Config.Tiers {
			for _, tm := range tierCfg.Models {
				if tm == m {
					rank := tierRank(tier)
					if rank > highestRank {
						highestRank = rank
						highestTier = tier
					}
				}
			}
		}
	}
	return highestTier
}

func tierFromRank(rank int) string {
	switch rank {
	case 0:
		return "free"
	case 1:
		return "mid"
	case 2:
		return "coder"
	case 3:
		return "premium"
	default:
		return "mid"
	}
}

func (e *Engine) determineComplexityTier(c *classify.Result) string {
	switch c.Complexity {
	case "trivial":
		return "free"
	case "simple":
		if c.TaskType == "coding" {
			return "coder"
		}
		return "free"
	case "moderate":
		if c.TaskType == "coding" {
			return "coder"
		}
		return "mid"
	case "complex", "expert":
		return "premium"
	default:
		return "mid"
	}
}

func (e *Engine) shouldStaySticky(c *classify.Result, s *session.Session, targetTier string) bool {
	if s == nil || s.Key == nil || !s.IsStickyEligible() {
		return false
	}

	if c.Complexity == "trivial" && c.ContextType == "standalone" {
		return false
	}

	if tierRank(targetTier) < tierRank(s.Tier) {
		if s.TurnCount >= e.Config.Economics.StickyMinTurns {
			return true
		}
		return false
	}

	if c.ContextType == "session_continuation" {
		return true
	}

	return false
}

func tierRank(tier string) int {
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

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

func (e *Engine) Decide(c *classify.Result, s *session.Session, agent string) *Decision {
	targetTier := e.determineTier(c, agent)
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

func (e *Engine) determineTier(c *classify.Result, agent string) string {
	if agentTier, ok := e.Config.AgentTiers[agent]; ok {
		tier := e.determineComplexityTier(c)
		if tierRank(agentTier) > tierRank(tier) {
			return agentTier
		}
		return tier
	}

	return e.determineComplexityTier(c)
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
		return 1
	case "premium":
		return 2
	default:
		return 1
	}
}

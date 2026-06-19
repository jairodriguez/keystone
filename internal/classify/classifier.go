package classify

import (
	"strings"
)

type Result struct {
	TaskType     string
	Complexity   string
	ContextType  string
	Source       string
}

type Classifier interface {
	Classify(prompt string, contextTokens int, turnCount int) *Result
}

type SimpleClassifier struct{}

func (c *SimpleClassifier) Classify(prompt string, contextTokens int, turnCount int) *Result {
	ctxType := "new_session"
	if turnCount > 0 {
		ctxType = "session_continuation"
	}
	return &Result{
		TaskType:    "conversation",
		Complexity:  "moderate",
		ContextType: ctxType,
		Source:      "bypass",
	}
}

type RuleClassifier struct{}

var (
	codeKeywords = []string{"function", "class", "bug", "error", "implement", "refactor", "test",
		"compile", "deploy", "api", "endpoint", "schema", "database", "query", "struct",
		"interface", "import", "package", "module", "configure"}
	extractKeywords = []string{"count", "list", "find", "search", "extract", "parse", "summarize data",
		"analyze file", "grep", "show all"}
	formatKeywords = []string{"format", "convert", "transform", "restructure", "reorder", "rename"}
	creativeKeywords = []string{"write a", "create a", "generate", "brainstorm", "compose", "design",
		"draft", "story", "poem", "article", "blog"}
)

func (c *RuleClassifier) Classify(prompt string, contextTokens int, turnCount int) *Result {
	lower := strings.ToLower(prompt)
	promptLen := len(prompt)

	taskType := "conversation"
	switch {
	case matchAny(lower, codeKeywords):
		taskType = "coding"
	case matchAny(lower, extractKeywords):
		taskType = "data_extraction"
	case matchAny(lower, formatKeywords):
		taskType = "formatting"
	case matchAny(lower, creativeKeywords):
		taskType = "creative"
	case promptLen < 60 && isQuestion(prompt):
		taskType = "simple_query"
	}

	complexity := "moderate"
	switch {
	case (contextTokens < 1000 || promptLen < 100) && taskType != "coding" && taskType != "creative":
		complexity = "trivial"
	case contextTokens < 5000 && taskType != "coding":
		complexity = "simple"
	case contextTokens > 100000 || turnCount > 5:
		complexity = "complex"
	case contextTokens > 150000:
		complexity = "expert"
	}

	ctxType := "new_session"
	if turnCount > 0 {
		ctxType = "session_continuation"
	}
	if turnCount == 0 && contextTokens < 1000 {
		ctxType = "standalone"
	}

	return &Result{
		TaskType:    taskType,
		Complexity:  complexity,
		ContextType: ctxType,
		Source:      "rule_based",
	}
}

func matchAny(s string, keywords []string) bool {
	for _, kw := range keywords {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

func isQuestion(prompt string) bool {
	return strings.HasSuffix(strings.TrimSpace(prompt), "?")
}

func Get(mode string) Classifier {
	switch mode {
	case "aggressive":
		return &RuleClassifier{}
	case "normal":
		return &RuleClassifier{}
	case "simple":
		return &SimpleClassifier{}
	default:
		return &SimpleClassifier{}
	}
}

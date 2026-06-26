package session

import (
	"testing"
	"time"

	"github.com/clawdbot/keystone/internal/keypool"
)

func newTestKey() *keypool.Key {
	return &keypool.Key{ID: "test-key", State: keypool.StateHealthy}
}

func coolingKey() *keypool.Key {
	return &keypool.Key{ID: "cooling-key", State: keypool.StateCooling}
}

func deadKey() *keypool.Key {
	return &keypool.Key{ID: "dead-key", State: keypool.StateDead}
}

func TestGetOrCreate_CreatesNew(t *testing.T) {
	s := NewMemoryStore()
	sess := s.GetOrCreate("abc", "agent1")
	if sess.ID != "abc" {
		t.Fatalf("expected id abc, got %s", sess.ID)
	}
	if sess.Agent != "agent1" {
		t.Fatalf("expected agent agent1, got %s", sess.Agent)
	}
	if sess.CreatedAt.IsZero() {
		t.Fatal("expected CreatedAt to be set")
	}
}

func TestGetOrCreate_ReturnsExisting(t *testing.T) {
	s := NewMemoryStore()
	s1 := s.GetOrCreate("abc", "agent1")
	s2 := s.GetOrCreate("abc", "agent2")
	if s1 != s2 {
		t.Fatal("expected same session pointer")
	}
	if s2.Agent != "agent1" {
		t.Fatal("expected agent not to be overwritten")
	}
}

func TestGet_NotFound(t *testing.T) {
	s := NewMemoryStore()
	if got := s.Get("nonexistent"); got != nil {
		t.Fatal("expected nil")
	}
}

func TestGet_Found(t *testing.T) {
	s := NewMemoryStore()
	s.GetOrCreate("abc", "a")
	if got := s.Get("abc"); got == nil {
		t.Fatal("expected session")
	}
}

func TestBind_CreatesIfNotExists(t *testing.T) {
	s := NewMemoryStore()
	k := newTestKey()
	s.Bind("new-sess", k, "zai", "glm-5.1", "premium")
	sess := s.Get("new-sess")
	if sess == nil {
		t.Fatal("expected session to be created")
	}
	if sess.Key.ID != "test-key" {
		t.Fatal("key mismatch")
	}
	if sess.Provider != "zai" || sess.Model != "glm-5.1" || sess.Tier != "premium" {
		t.Fatal("bind fields mismatch")
	}
}

func TestBind_UpdatesExisting(t *testing.T) {
	s := NewMemoryStore()
	s.GetOrCreate("abc", "a")
	k := newTestKey()
	s.Bind("abc", k, "nvidia", "nemotron-ultra", "premium")
	sess := s.Get("abc")
	if sess.Provider != "nvidia" || sess.Model != "nemotron-ultra" {
		t.Fatal("bind fields not updated")
	}
}

func TestUnbind(t *testing.T) {
	s := NewMemoryStore()
	k := newTestKey()
	s.Bind("abc", k, "zai", "glm-5.1", "premium")
	s.Unbind("abc")
	sess := s.Get("abc")
	if sess.Key != nil {
		t.Fatal("expected key nil after unbind")
	}
	if sess.Provider != "" || sess.Model != "" || sess.Tier != "" {
		t.Fatal("expected fields cleared")
	}
}

func TestUnbind_NoopOnMissing(t *testing.T) {
	s := NewMemoryStore()
	s.Unbind("nope") // should not panic
}

func TestIncrementTurn(t *testing.T) {
	s := NewMemoryStore()
	s.GetOrCreate("abc", "a")
	s.IncrementTurn("abc", 5000)
	sess := s.Get("abc")
	if sess.TurnCount != 1 {
		t.Fatalf("expected 1 turn, got %d", sess.TurnCount)
	}
	if sess.ContextEst != 5000 {
		t.Fatalf("expected ctx 5000, got %d", sess.ContextEst)
	}
	if sess.CacheHits != 0 {
		t.Fatal("expected 0 cache hits on first turn")
	}
}

func TestIncrementTurn_CacheHitsAfterFirst(t *testing.T) {
	s := NewMemoryStore()
	s.GetOrCreate("abc", "a")
	s.IncrementTurn("abc", 100)
	s.IncrementTurn("abc", 200)
	sess := s.Get("abc")
	if sess.TurnCount != 2 {
		t.Fatalf("expected 2 turns, got %d", sess.TurnCount)
	}
	if sess.CacheHits != 1 {
		t.Fatalf("expected 1 cache hit, got %d", sess.CacheHits)
	}
}

func TestIncrementTurn_NoopOnMissing(t *testing.T) {
	s := NewMemoryStore()
	s.IncrementTurn("nope", 100) // should not panic
}

func TestEvictExpired(t *testing.T) {
	s := NewMemoryStore()
	s.GetOrCreate("old", "a")
	s.GetOrCreate("new", "b")
	// Manually set old session's LastUsed far in the past
	s.sessions["old"].LastUsed = time.Now().Add(-2 * time.Hour)

	s.EvictExpired(time.Hour)
	if s.Get("old") != nil {
		t.Fatal("expected old session evicted")
	}
	if s.Get("new") == nil {
		t.Fatal("expected new session to remain")
	}
}

func TestEvictExpired_AllExpired(t *testing.T) {
	s := NewMemoryStore()
	s.GetOrCreate("s1", "a")
	s.GetOrCreate("s2", "b")
	s.sessions["s1"].LastUsed = time.Now().Add(-2 * time.Hour)
	s.sessions["s2"].LastUsed = time.Now().Add(-2 * time.Hour)
	s.EvictExpired(time.Hour)
	if s.ActiveCount() != 0 {
		t.Fatal("expected all sessions evicted")
	}
}

func TestActiveCount(t *testing.T) {
	s := NewMemoryStore()
	if c := s.ActiveCount(); c != 0 {
		t.Fatalf("expected 0, got %d", c)
	}
	s.GetOrCreate("a", "x")
	s.GetOrCreate("b", "y")
	if c := s.ActiveCount(); c != 2 {
		t.Fatalf("expected 2, got %d", c)
	}
}

func TestAll(t *testing.T) {
	s := NewMemoryStore()
	s.GetOrCreate("a", "x")
	s.GetOrCreate("b", "y")
	all := s.All()
	if len(all) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(all))
	}
}

func TestIsStickyEligible_NilKey(t *testing.T) {
	sess := &Session{}
	if sess.IsStickyEligible() {
		t.Fatal("expected false with nil key")
	}
}

func TestIsStickyEligible_HealthyKey(t *testing.T) {
	sess := &Session{Key: newTestKey()}
	if !sess.IsStickyEligible() {
		t.Fatal("expected true with healthy key")
	}
}

func TestIsStickyEligible_CoolingKey(t *testing.T) {
	sess := &Session{Key: coolingKey()}
	if sess.IsStickyEligible() {
		t.Fatal("expected false with cooling key")
	}
}

func TestIsStickyEligible_DeadKey(t *testing.T) {
	sess := &Session{Key: deadKey()}
	if sess.IsStickyEligible() {
		t.Fatal("expected false with dead key")
	}
}

// --- Manager tests ---

func TestManagerResolveSessionID_FromHeader(t *testing.T) {
	m := NewManager(NewMemoryStore(), time.Minute, "x-session-id", false, "x-agent")
	id := m.ResolveSessionID("custom-id", "")
	if id != "custom-id" {
		t.Fatalf("expected custom-id, got %s", id)
	}
}

func TestManagerResolveSessionID_AutoDerive(t *testing.T) {
	m := NewManager(NewMemoryStore(), time.Minute, "x-session-id", true, "x-agent")
	id := m.ResolveSessionID("", "agent:abc:123")
	if id == "default" || id == "" {
		t.Fatalf("expected derived id, got %s", id)
	}
}

func TestManagerResolveSessionID_Default(t *testing.T) {
	m := NewManager(NewMemoryStore(), time.Minute, "x-session-id", false, "x-agent")
	id := m.ResolveSessionID("", "")
	if id != "default" {
		t.Fatalf("expected default, got %s", id)
	}
}

func TestManagerGetOrCreate(t *testing.T) {
	m := NewManager(NewMemoryStore(), time.Minute, "x-session-id", true, "x-agent")
	sess := m.GetOrCreate("abc", "agent1")
	if sess.Agent != "agent1" {
		t.Fatal("agent mismatch")
	}
}

func TestManagerBindAndIncrementTurn(t *testing.T) {
	m := NewManager(NewMemoryStore(), time.Minute, "x-session-id", true, "x-agent")
	m.GetOrCreate("abc", "a")
	m.Bind("abc", newTestKey(), "zai", "glm-5.1", "premium")
	m.IncrementTurn("abc", 1000)
	sess := m.Get("abc")
	if sess.Tier != "premium" || sess.TurnCount != 1 {
		t.Fatal("bind + increment failed")
	}
}

func TestFormatSummary_Empty(t *testing.T) {
	m := NewManager(NewMemoryStore(), time.Minute, "x-session-id", true, "x-agent")
	summary := m.FormatSummary()
	if summary != "no active sessions" {
		t.Fatalf("expected 'no active sessions', got %q", summary)
	}
}

func TestFormatSummary_WithSessions(t *testing.T) {
	ms := NewMemoryStore()
	ms.GetOrCreate("sess-long-a", "agent1")
	m := NewManager(ms, time.Minute, "x-session-id", true, "x-agent")
	summary := m.FormatSummary()
	if len(summary) < 20 {
		t.Fatalf("expected detailed summary, got %q", summary)
	}
}

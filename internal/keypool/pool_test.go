package keypool

import (
	"testing"
	"time"
)

func TestNewPool(t *testing.T) {
	p := NewPool("test-provider", []string{"key-1", "key-2", "key-3"})
	if p.Provider != "test-provider" {
		t.Fatalf("expected provider test-provider, got %s", p.Provider)
	}
	if got := p.KeyCount(); got != 3 {
		t.Fatalf("expected 3 keys, got %d", got)
	}
}

func TestSelect_ReturnsLeastRecentlyUsed(t *testing.T) {
	p := NewPool("p", []string{"k1", "k2"})
	k1, _ := p.Select()
	k2, _ := p.Select()
	if k1.ID == k2.ID {
		t.Fatal("expected different keys on sequential selects")
	}
	// k2 was used most recently, so next Select should return k1
	k3, _ := p.Select()
	if k3.ID != k1.ID {
		t.Fatalf("expected LRU key %s, got %s", k1.ID, k3.ID)
	}
}

func TestSelect_ErrWhenAllDead(t *testing.T) {
	p := NewPool("p", []string{"k1"})
	k, _ := p.Select()
	p.TriggerCooldown(k.ID, 403, 0)
	_, err := p.Select()
	if err != ErrNoHealthyKeys {
		t.Fatalf("expected ErrNoHealthyKeys, got %v", err)
	}
}

func TestSelect_RevivesExpiredCooldown(t *testing.T) {
	p := NewPool("p", []string{"k1"})
	k1, _ := p.Select()
	p.TriggerCooldown(k1.ID, 429, -time.Hour) // negative duration = already expired
	k2, err := p.Select()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if k2.ID != k1.ID {
		t.Fatal("expected same key revived from cooldown")
	}
	if k2.State != StateHealthy {
		t.Fatal("expected key to be healthy after expiry")
	}
}

func TestSelectSpecific_Found(t *testing.T) {
	p := NewPool("p", []string{"k1"})
	k, _ := p.Select()
	got, err := p.SelectSpecific(k.ID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != k.ID {
		t.Fatal("key ID mismatch")
	}
}

func TestSelectSpecific_NotFound(t *testing.T) {
	p := NewPool("p", []string{"k1"})
	_, err := p.SelectSpecific("bogus")
	if err != ErrKeyNotFound {
		t.Fatalf("expected ErrKeyNotFound, got %v", err)
	}
}

func TestTriggerCooldown_401MarksDead(t *testing.T) {
	p := NewPool("p", []string{"k1"})
	k, _ := p.Select()
	p.TriggerCooldown(k.ID, 401, 0)
	if k.State != StateDead {
		t.Fatal("expected key dead after 401")
	}
	if k.Errors != 1 {
		t.Fatalf("expected 1 error, got %d", k.Errors)
	}
}

func TestTriggerCooldown_429MarksCooling(t *testing.T) {
	p := NewPool("p", []string{"k1"})
	k, _ := p.Select()
	p.TriggerCooldown(k.ID, 429, 5*time.Second)
	if k.State != StateCooling {
		t.Fatal("expected key cooling after 429")
	}
	if k.Last429.IsZero() {
		t.Fatal("expected Last429 to be set")
	}
}

func TestTriggerCooldown_500MarksCooling(t *testing.T) {
	p := NewPool("p", []string{"k1"})
	k, _ := p.Select()
	p.TriggerCooldown(k.ID, 502, 10*time.Second)
	if k.State != StateCooling {
		t.Fatal("expected key cooling after 502")
	}
}

func TestTriggerCooldown_400MarksCooling(t *testing.T) {
	p := NewPool("p", []string{"k1"})
	k, _ := p.Select()
	p.TriggerCooldown(k.ID, 400, 30*time.Second)
	if k.State != StateCooling {
		t.Fatal("expected key cooling after 400 (context length / unknown model)")
	}
}

func TestTriggerCooldown_NoopOnUnknownKey(t *testing.T) {
	p := NewPool("p", []string{"k1"})
	p.TriggerCooldown("bogus", 429, 0) // should not panic
	if k := p.AllKeys()[0]; k.State != StateHealthy {
		t.Fatal("expected key unaffected")
	}
}

func TestRecoverExpired(t *testing.T) {
	p := NewPool("p", []string{"k1"})
	k, _ := p.Select()
	p.TriggerCooldown(k.ID, 429, -time.Hour)
	p.RecoverExpired()
	if k.State != StateHealthy {
		t.Fatal("expected key recovered after expired cooldown")
	}
}

func TestRecoverExpired_IgnoresActiveCooldown(t *testing.T) {
	p := NewPool("p", []string{"k1"})
	k, _ := p.Select()
	p.TriggerCooldown(k.ID, 429, time.Hour)
	p.RecoverExpired()
	if k.State != StateCooling {
		t.Fatal("expected key still cooling")
	}
}

func TestIsHealthy_TrueWithHealthyKey(t *testing.T) {
	p := NewPool("p", []string{"k1"})
	if !p.IsHealthy() {
		t.Fatal("expected pool healthy")
	}
}

func TestIsHealthy_FalseAllDead(t *testing.T) {
	p := NewPool("p", []string{"k1"})
	k, _ := p.Select()
	p.TriggerCooldown(k.ID, 403, 0)
	if p.IsHealthy() {
		t.Fatal("expected pool unhealthy")
	}
}

func TestIsHealthy_TrueWithExpiredCooldown(t *testing.T) {
	p := NewPool("p", []string{"k1"})
	k, _ := p.Select()
	p.TriggerCooldown(k.ID, 429, -time.Hour) // expired
	if !p.IsHealthy() {
		t.Fatal("expected pool healthy with expired cooling key")
	}
}

func TestHealthyCount(t *testing.T) {
	p := NewPool("p", []string{"k1", "k2"})
	k1, _ := p.Select()
	p.TriggerCooldown(k1.ID, 403, 0) // k1 dead
	if got := p.HealthyCount(); got != 1 {
		t.Fatalf("expected 1 healthy key, got %d", got)
	}
}

func TestAllKeys_ReturnsSliceCopy(t *testing.T) {
	p := NewPool("p", []string{"k1", "k2"})
	keys := p.AllKeys()
	// Appending to returned slice should not affect original pool
	keys = append(keys, &Key{ID: "extra"})
	if p.KeyCount() != 2 {
		t.Fatal("AllKeys returned a copy; appending should not affect pool")
	}
}

func TestKeyWeightDefault(t *testing.T) {
	// newKey with weight 0 should default to 1
	p := NewPool("p", []string{"k1"})
	k := p.AllKeys()[0]
	if k.Weight != 1 {
		t.Fatalf("expected weight 1, got %d", k.Weight)
	}
}

func TestKeyStateString(t *testing.T) {
	cases := []struct {
		s    KeyState
		want string
	}{
		{StateHealthy, "healthy"},
		{StateCooling, "cooling"},
		{StateDead, "dead"},
		{KeyState(99), "unknown"},
	}
	for _, tc := range cases {
		if got := tc.s.String(); got != tc.want {
			t.Errorf("StateString(%d) = %q, want %q", tc.s, got, tc.want)
		}
	}
}

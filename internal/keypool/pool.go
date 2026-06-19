package keypool

import (
	"crypto/sha256"
	"fmt"
	"sync"
	"time"
)

type KeyState int

const (
	StateHealthy KeyState = iota
	StateCooling
	StateDead
)

func (s KeyState) String() string {
	switch s {
	case StateHealthy:
		return "healthy"
	case StateCooling:
		return "cooling"
	case StateDead:
		return "dead"
	default:
		return "unknown"
	}
}

type Key struct {
	ID            string
	Value         string
	Provider      string
	State         KeyState
	CooldownUntil time.Time
	Requests      int64
	Errors        int64
	Last429       time.Time
	LastUsed      time.Time
	Weight        int
}

func newKey(value, provider string, weight int) *Key {
	h := sha256.Sum256([]byte(value))
	id := fmt.Sprintf("...%s", fmt.Sprintf("%x", h[len(h)-2:]))
	if weight <= 0 {
		weight = 1
	}
	return &Key{
		ID:       id,
		Value:    value,
		Provider: provider,
		State:    StateHealthy,
		Weight:   weight,
	}
}

type Pool struct {
	Provider string
	keys     []*Key
	mu       sync.RWMutex
}

func NewPool(provider string, keyValues []string) *Pool {
	p := &Pool{Provider: provider}
	for _, kv := range keyValues {
		p.keys = append(p.keys, newKey(kv, provider, 1))
	}
	return p
}

func (p *Pool) Select() (*Key, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	var candidates []*Key
	for _, k := range p.keys {
		if k.State == StateCooling && now.After(k.CooldownUntil) {
			k.State = StateHealthy
		}
		if k.State == StateHealthy {
			candidates = append(candidates, k)
		}
	}

	if len(candidates) == 0 {
		return nil, ErrNoHealthyKeys
	}

	least := candidates[0]
	for _, k := range candidates[1:] {
		if k.LastUsed.Before(least.LastUsed) {
			least = k
		}
	}
	least.LastUsed = now
	least.Requests++
	return least, nil
}

func (p *Pool) SelectSpecific(keyID string) (*Key, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, k := range p.keys {
		if k.ID == keyID {
			return k, nil
		}
	}
	return nil, ErrKeyNotFound
}

func (p *Pool) TriggerCooldown(keyID string, statusCode int, duration time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, k := range p.keys {
		if k.ID != keyID {
			continue
		}
		k.Errors++
		switch statusCode {
		case 401, 403:
			k.State = StateDead
		case 429:
			k.State = StateCooling
			k.CooldownUntil = time.Now().Add(duration)
			k.Last429 = time.Now()
		case 500, 502, 503:
			k.State = StateCooling
			k.CooldownUntil = time.Now().Add(duration)
		}
		return
	}
}

func (p *Pool) IsHealthy() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	for _, k := range p.keys {
		if k.State == StateCooling && now.After(k.CooldownUntil) {
			return true
		}
		if k.State == StateHealthy {
			return true
		}
	}
	return false
}

func (p *Pool) KeyCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.keys)
}

func (p *Pool) HealthyCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	count := 0
	for _, k := range p.keys {
		if k.State == StateCooling && now.After(k.CooldownUntil) {
			count++
		} else if k.State == StateHealthy {
			count++
		}
	}
	return count
}

func (p *Pool) AllKeys() []*Key {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*Key, len(p.keys))
	copy(out, p.keys)
	return out
}

func (p *Pool) RecoverExpired() {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for _, k := range p.keys {
		if k.State == StateCooling && now.After(k.CooldownUntil) {
			k.State = StateHealthy
		}
	}
}

var (
	ErrNoHealthyKeys = fmt.Errorf("no healthy keys available")
	ErrKeyNotFound   = fmt.Errorf("key not found")
)

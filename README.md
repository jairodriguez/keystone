# Keystone

> **Session-sticky AI API proxy with multi-key rotation, KV cache preservation, cross-provider fallback, and intelligent model tier selection.**

[![Go Version](https://img.shields.io/badge/Go-1.22+-00ADD8?logo=go)](https://golang.org)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Build Status](https://github.com/clawdbot/keystone/workflows/CI/badge.svg)](https://github.com/clawdbot/keystone/actions)

Keystone is a drop-in OpenAI-compatible proxy that routes requests across multiple AI providers (Z.ai, NVIDIA, GitHub Copilot, etc.) with:

- **Session stickiness** — Preserves KV cache by reusing the same key for continued conversations
- **Multi-key rotation** — Automatic load balancing with health checks and cooldowns
- **Cross-provider fallback** — Premium → Mid → Free tier automatic failover
- **Intelligent tier selection** — Economic engine chooses tier based on task complexity and cache value
- **Agent-aware routing** — Per-agent tier floors from config
- **OpenAI-compatible** — Drop-in replacement, no SDK changes needed

## Architecture

```
┌─────────────┐     ┌──────────────────┐     ┌────────────────────┐
│   Client    │────▶│    Keystone      │────▶│  Provider Pool     │
│ (OpenAI SDK)│     │  (This Proxy)    │     │  ┌──────────────┐  │
└─────────────┘     └──────────────────┘     │  │ Z.ai (GLM)   │  │
                        │                    │  ├──────────────┤  │
                        ▼                    │  │ NVIDIA       │  │
                 ┌─────────────┐             │  ├──────────────┤  │
                 │  Economics  │             │  │ GitHub       │  │
                 │   Engine    │             │  │ Copilot      │  │
                 └─────────────┘             │  └──────────────┘  │
                        │                    └────────────────────┘
                        ▼
                 ┌─────────────┐
                 │   Session   │
                 │  Manager    │
                 └─────────────┘
```

## Quickstart

### 1. Get Free Model API Keys

| Provider | Model(s) | How to Get Key |
|----------|----------|----------------|
| **Z.ai** | GLM-5.1, GLM-4.5, GLM-4.5-Air | Sign up at [z.ai](https://z.ai) → API Keys |
| **NVIDIA** | Nemotron-3-Ultra, Llama-3.1-Nemotron | [NVIDIA API Catalog](https://build.nvidia.com/explore/discover) → Get API Key |
| **GitHub Copilot** | GPT-4o, GPT-4o-mini, Claude-3.5-Sonnet | Enable [Copilot in CLI](https://github.com/github/copilot-cli) → `gh auth token` |

### 2. Configure

```bash
# Clone and configure
git clone https://github.com/clawdbot/keystone
cd keystone
cp config/keystone.example.yaml config/keystone.yaml

# Edit with your keys (or use env vars)
vim config/keystone.yaml
```

### 3. Run

```bash
# Using Make
make build && bin/keystone

# Or with Docker
docker build -t keystone -f docker/Dockerfile .
docker run -p 8080:8080 \
  -e ZAI_API_KEY_1=your_key \
  -e NVIDIA_API_KEY_1=your_key \
  -e GITHUB_COPILOT_KEY_1=your_key \
  keystone
```

### 4. Use

```bash
# Drop-in OpenAI SDK usage
export OPENAI_BASE_URL=http://localhost:8080/v1
export OPENAI_API_KEY=keystone  # any non-empty string

# Python
from openai import OpenAI
client = OpenAI()
client.chat.completions.create(
    model="glm-5.1",
    messages=[{"role": "user", "content": "Hello!"}]
)

# TypeScript
import OpenAI from "openai";
const client = new OpenAI({ baseURL: "http://localhost:8080/v1", apiKey: "keystone" });
```

## Configuration

All settings in `config/keystone.yaml` (see [example](config/keystone.example.yaml)):

| Section | Purpose |
|---------|---------|
| `server` | HTTP listen address, timeouts |
| `sessions` | TTL, header names, auto-derive session ID |
| `classifier` | Mode: `aggressive` \| `normal` \| `simple` |
| `providers` | Provider list with keys, models, cooldowns |
| `tiers` | Tier → model mappings |
| `fallback` | Cross-tier fallback + provider chains |
| `model_map` | Requested model → provider-specific name |
| `agent_tiers` | Per-agent minimum tier floor |
| `economics` | Stickiness thresholds, cache hit ratio |
| `metrics` | Prometheus `/metrics` endpoint |
| `api` | Control API (`/api/mode`, `/api/stats`, `/api/sessions`) |

### Classifier Modes

| Mode | Behavior |
|------|----------|
| `aggressive` | Rule-based, maximizes free tier usage |
| `normal` | Balanced rule-based classification |
| `simple` | Bypass classifier, use requested model directly |

### Agent Tier Floors

Agents declare themselves via `x-agent` header. Config maps agent → minimum tier:

```yaml
agent_tiers:
  "coder": "coder"        # Can use coder, mid, premium
  "researcher": "mid"     # Can use mid, premium
  "default": "free"       # Fallback for unknown agents
```

## Control API

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/mode` | GET | Current classifier mode |
| `/api/mode` | POST | Set mode (`{"mode": "aggressive"}`) |
| `/api/health` | GET | Health check |
| `/api/stats` | GET | Provider/key/session stats |
| `/api/sessions` | GET | List active sessions |
| `/api/sessions/{id}/unbind` | POST | Force unbind session |

## Monitoring

Prometheus metrics at `/metrics`:

```
keystone_requests_total{provider,tier,model,status}
keystone_request_duration_seconds{provider,tier}
keystone_tokens_used_total{provider,tier}
keystone_active_sessions
keystone_keys_healthy{provider}
keystone_keys_cooling{provider}
keystone_keys_dead{provider}
keystone_fallbacks_total{from_provider,to_provider,tier}
keystone_tier_switches_total{from_tier,to_tier,reason}
```

## How It Works

### Request Flow

1. **Classify** — Analyze prompt complexity, task type, context size
2. **Economics** — Decide tier + stickiness (cache savings vs. cost)
3. **Route** — Select provider + key from tier's fallback chain
4. **Sticky Bind** — If eligible, reuse session's key for KV cache
5. **Execute** — Proxy request with SSE streaming
6. **Fallback** — On 429/5xx/401, trigger cooldown → try next provider/tier
7. **Metrics** — Record latency, tokens, fallbacks, tier switches

### Stickiness Logic

A session becomes "sticky" when:
- `turn_count >= 3` (configurable) **OR**
- `context_tokens >= 50000` (configurable)

Sticky sessions reuse the same key to preserve KV cache. The economics engine calculates whether cache savings justify staying on a higher tier vs. falling back to a cheaper one.

### Key Health States

```
Healthy ──(429)──▶ Cooling (60s) ──▶ Healthy
   │
   ├─(500/502/503)──▶ Cooling (10s) ──▶ Healthy
   │
   └─(401/403)──▶ Dead (permanent)
```

## Benchmarks

_TODO: Add benchmark results after load testing_

Target: <5ms proxy overhead at P99, 10k+ RPS on modest hardware.

## Development

```bash
# Install tools
make install-tools

# Dev cycle
make dev

# Run tests with coverage
make test-coverage

# Build Docker
make docker
```

## License

Apache 2.0 — see [LICENSE](LICENSE)

## Contributing

1. Fork
2. Create feature branch
3. Run `make dev` (tidy, fmt, vet, test, build)
4. Submit PR

## Roadmap

- [ ] Phase 2: CTX MCP integration + Gemini Flash classifier
- [ ] Redis-backed distributed session store
- [ ] Admin dashboard (React + SSE)
- [ ] Request/response logging with PII redaction
- [ ] Custom routing rules DSL
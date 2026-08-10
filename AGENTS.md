# llm-slo-bench

A single-binary Go load generator and SLO gate for OpenAI-compatible LLM endpoints.

## What this is

Measures what generic load tools cannot: **time-to-first-token (TTFT), inter-token latency (ITL), tokens/sec, and cost per request, parsed from live SSE streams under concurrent load**. Runs ramp, burst, and failover-drill scenarios. Evaluates results against YAML-declared SLOs (for example `p99_ttft_ms: 800`) and returns pass/fail exit codes so it works as a CI gate. Outputs self-contained HTML plus JSON reports.

Companion project: the owner's existing `sre-inference-gateway` (github.com/crypticseeds/sre-inference-gateway, Python/FastAPI, multi-provider LLM gateway with circuit breaking, chaos injection, Redis quotas, Prometheus/OTel). On Day 6 this tool runs against that gateway and the measured numbers get published in both READMEs. The two repos form one portfolio narrative: *built the inference control plane, built the verification tooling, here is how it behaves at p99 under provider failure.*

## Owner context

- Femi Akinlotan (github.com/crypticseeds), London. Positioning: **SRE / Platform Engineer for AI inference (Go, Python, K8s, AWS)**. Actively job hunting; this project is the week's portfolio centerpiece.
- Femi is **learning Go**. Agents do most of the building, but one small, high-value component is reserved for Femi to hand-code with agent pairing: **the config loader + SLO evaluator pure logic** (`Config.Validate` and `ComparePercentile`). Agents build the SSE/TTFT measurement core and explain it through focused comments and `docs/MEASUREMENT.md`.
- Time budget: afternoon block 2h30 per day for ~7 days, evening spillover possible after the time off ends. Target: **80-90% complete within the week**, releasable as v0.1.0 as fast as possible after.

## Hard scope rules

- **Bare minimum, done well. No scope creep.** Anything not needed for v0.1.0 goes on a LATER list in `docs/LATER.md`, not into code.
- v0.1.0 = streaming metrics (TTFT/ITL/TPS/cost) + ramp and burst scenarios + YAML SLO gate with exit codes + JSON and self-contained HTML reports + failover drill + single binary (goreleaser) + Docker image + README with measured gateway numbers.
- Explicitly OUT of v0.1.0: database server, web UI/dashboard, multi-node distributed load, provider SDKs beyond OpenAI-compatible HTTP, auth systems, plugins, TUI, soak scenario (only if ahead of schedule), Kubernetes operator.
- Default storage decision (challenge in kickoff if wrong): **no database**. In-memory histograms + per-request timing records appended to JSONL; reports read JSONL. Raw response bodies are NOT stored by default (size + privacy); store timings, token counts, status, error taxonomy.
- Day 2 gate: a TTFT histogram from one streaming endpoint under concurrent load, computed by our own code. If the gate fails, cut scenarios to ramp-only and keep the SLO gate. Never switch projects.
- Prior art honesty: llmperf (Anyscale) and genai-perf (NVIDIA) exist. Differentiators: Go single static binary, CI-gating SLOs with exit codes, failover drills, provider-agnostic. Judged on rigor, not novelty.

## Working rules

- Primary test target during development: the built-in **`llm-slo-bench mock`** subcommand - deterministic and zero spend. The gateway mock is the Day 6 integration target. Real providers are only for short validation runs.
- Go module path: `github.com/crypticseeds/llm-slo-bench`. MIT license. Standard library first; each new dependency needs a one-line justification in the PR/commit.
- Every work package ends with tests and a runnable demo command. Unverified work is not done.
- Femi's learning: when touching the measurement core, explain design choices (goroutines, channels, monotonic clock, SSE framing) rather than silently writing code.
- Progress tracking lives only in `BOARD.md`. Keep each stream's owner and `todo` / `building` / `review` / `done` status current; the coordinator moves reviewed streams to `done`.

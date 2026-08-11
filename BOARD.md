# Day 1 Build Board

Status values: `todo`, `building`, `review`, `done`. Definitions of done are in [`../llm-slo-bench-adr-v1.html`](../llm-slo-bench-adr-v1.html), section 08.

| Stream | Owner | Status | DoD reference |
| --- | --- | --- | --- |
| A - Config loader + SLO evaluator | Femi | building | ADR v1 section 08, stream A; unblocked: parser/types/tests and corrected contract ready, `Config.Validate` and `ComparePercentile` remain |
| B - Built-in mock SSE server | agent | done | ADR v1 section 08, stream B |
| C - SSE client + semantic timing | agent | done | ADR v1 section 08, stream C; record TTFB and semantic TTFT, gate only TTFT; compression disabled |
| D - Open-loop scheduler | agent | review | ADR v1 section 08, stream D; includes deferred mockserver `BaseContext` shutdown wiring and response-header timeout |
| E - Metrics aggregation | agent | review | ADR v1 section 08, stream E |
| F - CLI + reports (Day 1 probe slice) | agent | done | ADR v1 section 08, stream F; reports remain future work |
| G - Release + CI | agent | todo | ADR v1 section 08, stream G |
| H - Gateway evidence | agent | todo | ADR v1 section 08, stream H |

## Cut Notes

- Day 1 is limited to streams A, B, C, and the probe slice of F. D, E, G, and H remain untouched per the ADR cut lines.
- Four tests fail from two root-cause TODO stubs: `TestValidateContractForFemi` depends on `Config.Validate`; `TestComparePercentileContractForFemi`, `TestEvaluateGatesSemanticTTFTAndNotTTFB`, and `TestEvaluateIncludesAllConfiguredGatesAndSampleCounts` depend on `ComparePercentile`. Femi is unblocked; all agent-owned tests and build checks are green.
- LATER: document that callers of `mockserver.NewHandler` must pass a validated config.

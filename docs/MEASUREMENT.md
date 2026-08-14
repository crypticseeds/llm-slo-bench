# Streaming Measurement Core

The probe uses one request. `http.Client` manages network I/O internally,
and `time.AfterFunc` runs the idle-timeout callback in its own goroutine. That
callback sends to a one-slot buffered `idleExpired` channel so it never blocks
while the request goroutine is still inside a body read. The ramp scenario runs
bounded request goroutines and returns immutable results to one aggregator;
that keeps histogram mutation off the timing path and avoids shared locks.

## Clock Points

Go `time.Time` values retain a monotonic component when created with
`time.Now`. The probe stamps `Result.Dispatch` immediately before
`http.Client.Do` and computes all durations from that same clock point, so
wall-clock adjustments cannot skew the run. `Result.Duration` is populated on
both successful `[DONE]` completion and failures after dispatch.

- **TTFB**: `GotFirstResponseByte - start`, captured by `httptrace`. This is a
  transport diagnostic and is never an SLO gate.
- **Semantic TTFT**: first non-empty `choices[].delta.content - start`.
  Role-only, usage-only, keepalive, and empty-content events do not count.
- **Chunk ITL**: arrival interval between consecutive non-empty content events.
  It is deliberately named chunk ITL because an SSE event may contain zero,
  one, or many model tokens.

## SSE Framing

HTTP read boundaries are not SSE event boundaries. The decoder reads UTF-8
lines, accepts CRLF, LF, and bare CR, ignores comment lines, joins repeated
`data:` fields with a newline, and dispatches only at a blank line. It does not
use `bufio.Scanner`, whose default token limit can reject a large event.

An idle timer is reset after every complete SSE event. Total timeout and parent
cancellation use the request context, which closes the response body and
unblocks reads. Malformed JSON, premature EOF, or a stream with no semantic
content is a request failure. Probe errors wrap stable sentinels while retaining
their existing human-readable text, allowing callers to classify failures with
`errors.Is` without parsing response text.

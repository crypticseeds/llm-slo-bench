# Streaming Measurement Core

The Day 1 probe uses one request. `http.Client` manages network I/O internally,
and `time.AfterFunc` runs the idle-timeout callback in its own goroutine. That
callback sends to a one-slot buffered `idleExpired` channel so it never blocks
while the request goroutine is still inside a body read. Later load scenarios
will run bounded request goroutines and send immutable results over a channel
to one aggregator; that keeps histogram mutation off the timing path and avoids
shared locks.

## Clock Points

Go `time.Time` values retain a monotonic component when created with
`time.Now`. The probe stamps `start` immediately before `http.Client.Do` and
computes all durations by subtraction, so wall-clock adjustments cannot skew
the run.

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
content is a request failure.

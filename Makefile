.PHONY: demo-html

demo-html:
	@mkdir -p bin artifacts
	@go build -o bin/llm-slo-bench ./cmd/llm-slo-bench
	@set -eu; \
		./bin/llm-slo-bench mock --profile fast >artifacts/mock.log 2>&1 & mock_pid=$$!; \
		trap 'kill $$mock_pid 2>/dev/null || true; wait $$mock_pid 2>/dev/null || true' EXIT INT TERM; \
		ready=0; \
		for attempt in 1 2 3 4 5 6 7 8 9 10; do \
			if ! kill -0 $$mock_pid 2>/dev/null; then printf '%s\n' 'mock exited before becoming ready' >&2; exit 1; fi; \
			if ./bin/llm-slo-bench probe --timeout 500ms >/dev/null 2>&1; then \
				sleep 0.1; \
				if ! kill -0 $$mock_pid 2>/dev/null; then printf '%s\n' 'mock exited after readiness probe' >&2; exit 1; fi; \
				ready=1; break; \
			fi; \
			sleep 0.1; \
		done; \
		if [ $$ready -ne 1 ]; then printf '%s\n' 'mock did not become ready' >&2; exit 1; fi; \
		./bin/llm-slo-bench ramp --config examples/quickstart.yaml --out artifacts/demo.json --html artifacts/demo.html; \
		printf '%s\n' "$$(pwd)/artifacts/demo.html"

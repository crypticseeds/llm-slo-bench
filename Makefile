BINARY := bin/llm-slo-bench
VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test race demo demo-html

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/llm-slo-bench

test:
	go test ./...

race:
	go test -race ./...

demo: build
	@set -eu; \
		if ! command -v python3 >/dev/null 2>&1; then printf '%s\n' 'python3 is required to check port 18080' >&2; exit 1; fi; \
		if ! python3 -c 'import socket; listener = socket.socket(); listener.bind(("127.0.0.1", 18080)); listener.close()' >/dev/null 2>&1; then printf '%s\n' 'port 18080 is already in use' >&2; exit 1; fi; \
		$(BINARY) mock --listen 127.0.0.1:18080 >/dev/null 2>&1 & mock_pid=$$!; \
		trap 'kill $$mock_pid 2>/dev/null || true; wait $$mock_pid 2>/dev/null || true' EXIT INT TERM; \
		ready=0; \
		for attempt in 1 2 3 4 5 6 7 8 9 10; do \
			if ! kill -0 $$mock_pid 2>/dev/null; then printf '%s\n' 'mock exited before becoming ready' >&2; exit 1; fi; \
			if $(BINARY) probe --endpoint http://127.0.0.1:18080/v1/chat/completions --timeout 500ms >/dev/null 2>&1; then \
				ready=1; break; \
			fi; \
			sleep 0.1; \
		done; \
		if [ $$ready -ne 1 ]; then printf '%s\n' 'mock did not become ready' >&2; exit 1; fi; \
		$(BINARY) probe --endpoint http://127.0.0.1:18080/v1/chat/completions

demo-html:
	@mkdir -p bin artifacts
	@go build -o bin/llm-slo-bench ./cmd/llm-slo-bench
	@set -eu; \
		if ! command -v python3 >/dev/null 2>&1; then printf '%s\n' 'python3 is required to check port 8080' >&2; exit 1; fi; \
		if ! python3 -c 'import socket; listener = socket.socket(); listener.bind(("127.0.0.1", 8080)); listener.close()' >/dev/null 2>&1; then printf '%s\n' 'port 8080 is already in use' >&2; exit 1; fi; \
		./bin/llm-slo-bench mock --profile fast >artifacts/mock.log 2>&1 & mock_pid=$$!; \
		trap 'kill $$mock_pid 2>/dev/null || true; wait $$mock_pid 2>/dev/null || true' EXIT INT TERM; \
		ready=0; \
		for attempt in 1 2 3 4 5 6 7 8 9 10; do \
			if ! kill -0 $$mock_pid 2>/dev/null; then printf '%s\n' 'mock exited before becoming ready' >&2; exit 1; fi; \
			if ./bin/llm-slo-bench probe --timeout 500ms >/dev/null 2>&1; then \
				ready=1; break; \
			fi; \
			sleep 0.1; \
		done; \
		if [ $$ready -ne 1 ]; then printf '%s\n' 'mock did not become ready' >&2; exit 1; fi; \
		./bin/llm-slo-bench ramp --config examples/quickstart.yaml --out artifacts/demo.json --html artifacts/demo.html; \
		printf '%s\n' "$$(pwd)/artifacts/demo.html"

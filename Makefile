BINARY := bin/llm-slo-bench
VERSION ?= dev
LDFLAGS := -s -w -X runtime.buildVersion=$(VERSION)

TEST_PACKAGES := ./cmd/... ./internal/probe ./internal/mockserver ./internal/loadgen ./internal/metrics
ifneq ($(wildcard internal/report),)
TEST_PACKAGES += ./internal/report
endif

.PHONY: build test race demo

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/llm-slo-bench

test:
	go test $(TEST_PACKAGES)
	# Enable once the owner-owned Config.Validate and ComparePercentile implementations land:
	# go test ./...

race:
	go test -race $(TEST_PACKAGES)
	# Enable once the owner-owned Config.Validate and ComparePercentile implementations land:
	# go test -race ./...

demo: build
	@set -eu; \
	  $(BINARY) mock --listen 127.0.0.1:18080 >/dev/null 2>&1 & \
	  mock_pid=$$!; \
	  trap 'kill $$mock_pid 2>/dev/null || true; wait $$mock_pid 2>/dev/null || true' EXIT INT TERM; \
	  sleep 1; \
	  $(BINARY) probe --endpoint http://127.0.0.1:18080/v1/chat/completions

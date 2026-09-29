GO      ?= go
BIN     := bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/baruahs262/container-runtime/internal/cli.Version=$(VERSION)

export CGO_ENABLED = 0

.PHONY: all build test unit integration bench vet fmt clean

all: build

build:
	$(GO) build -ldflags '$(LDFLAGS)' -o $(BIN)/crt ./cmd/crt
	$(GO) build -ldflags '-s -w' -o $(BIN)/crt-stress ./cmd/crt-stress

vet:
	$(GO) vet ./...
	@test -z "$$(gofmt -l .)" || (gofmt -l .; echo "gofmt needed"; exit 1)

# Unit tests run anywhere; root-only tests skip themselves.
unit:
	$(GO) test ./...

# Everything, including namespace/cgroup/overlay tests (needs root).
test: build
	sudo -E env "PATH=$$PATH" $(GO) test -count=1 ./...

integration: build
	sudo -E env "PATH=$$PATH" $(GO) test -count=1 -v ./test/integration/

bench: build
	sudo $(BIN)/crt bench | tee bench-results.md

clean:
	rm -rf $(BIN) bench-results.md

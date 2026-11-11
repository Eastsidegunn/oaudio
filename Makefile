VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
BIN := bin
UNAME := $(shell uname -s)

.PHONY: all build helpers runners test e2e clean

all: build helpers

# The daemon and CLI: pure Go, standard library only, no cgo.
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/oaudio ./cmd/oaudio

# Helpers for engines built into the binary. Only macOS has one.
helpers:
ifeq ($(UNAME),Darwin)
	swiftc -O -o $(BIN)/oaudio-apple-stt internal/engines/apple/stt.swift
endif

# Engine runners. These compile third-party engines (whisper.cpp and
# audio.cpp from source, sherpa-onnx from its prebuilt libraries), so they
# need a C/C++ toolchain and cmake, and are kept out of the default target.
# Each can be built alone: `make -C runners/<name> OUT=$(abspath $(BIN))`.
runners:
	$(MAKE) -C runners/whisper OUT=$(abspath $(BIN))
	$(MAKE) -C runners/sherpa OUT=$(abspath $(BIN))
	$(MAKE) -C runners/audiocpp OUT=$(abspath $(BIN))

test:
	test -z "$$(gofmt -l cmd internal runner runners/whisper/main.go runners/sherpa runners/audiocpp)"
	go vet ./...
	go test ./...
	cd runners/audiocpp && go vet . && go test .

# End-to-end check against the real binary and the real system engines.
e2e: all
	./scripts/e2e.sh

clean:
	rm -rf $(BIN)

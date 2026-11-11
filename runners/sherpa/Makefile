# Builds oaudio-runner-sherpa-onnx and places the sherpa-onnx and ONNX Runtime
# shared libraries it needs in a lib/ directory beside it, so the pair can be
# copied to another machine. The libraries come prebuilt from the
# sherpa-onnx-go module pinned in go.mod.
OUT ?= ../../bin
ifeq ($(shell uname -s),Darwin)
MACOSX_DEPLOYMENT_TARGET ?= 14.0
# Named in the flags, not only the environment, so that Go's build cache
# cannot reuse C objects compiled for another macOS version.
export CGO_CFLAGS ?= -O2 -g -mmacosx-version-min=$(MACOSX_DEPLOYMENT_TARGET)
export CGO_LDFLAGS ?= -mmacosx-version-min=$(MACOSX_DEPLOYMENT_TARGET)
endif

.PHONY: all
all:
	go mod download
	CGO_ENABLED=1 go build -trimpath -ldflags "-s -w" -o $(OUT)/oaudio-runner-sherpa-onnx .
	./bundle-libs.sh $(OUT)

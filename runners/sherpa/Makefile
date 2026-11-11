# Builds oaudio-runner-sherpa-onnx and places the sherpa-onnx and ONNX Runtime
# shared libraries it needs in a lib/ directory beside it, so the pair can be
# copied to another machine. The libraries come prebuilt from the
# sherpa-onnx-go module pinned in go.mod.
OUT ?= ../../bin

.PHONY: all
all:
	go mod download
	CGO_ENABLED=1 go build -trimpath -ldflags "-s -w" -o $(OUT)/oaudio-runner-sherpa-onnx .
	./bundle-libs.sh $(OUT)

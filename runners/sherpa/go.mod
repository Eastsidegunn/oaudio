module github.com/eastsidegunn/oaudio/runners/sherpa

go 1.24

require (
	github.com/eastsidegunn/oaudio v0.0.0
	github.com/k2-fsa/sherpa-onnx-go/v2 v2.0.1
)

require (
	github.com/k2-fsa/sherpa-onnx-go-linux/v2 v2.0.1 // indirect
	github.com/k2-fsa/sherpa-onnx-go-macos/v2 v2.0.1 // indirect
	github.com/k2-fsa/sherpa-onnx-go-windows/v2 v2.0.1 // indirect
)

replace github.com/eastsidegunn/oaudio => ../..

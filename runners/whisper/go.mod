module github.com/eastsidegunn/oaudio/runners/whisper

go 1.24

require (
	github.com/eastsidegunn/oaudio v0.0.0
	github.com/ggerganov/whisper.cpp/bindings/go v0.0.0
)

// Both are resolved locally by `make` in this directory: oaudio from the
// repository root, and the bindings from the whisper.cpp checkout it builds.
replace github.com/eastsidegunn/oaudio => ../..

replace github.com/ggerganov/whisper.cpp/bindings/go => ./third_party/whisper.cpp/bindings/go

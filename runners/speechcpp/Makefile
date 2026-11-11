# Builds oaudio-runner-speech.cpp and the speech.cpp command it drives
# (`speech worker`), placing both in OUT. Needs git, cmake and a C++17
# compiler. speech.cpp's executable is called `speech` upstream; it is
# installed as `speechcpp` so that the name says what it is.
SPEECHCPP_TAG ?= v0.10.1
SRC := third_party/speech.cpp
BUILD := $(SRC)/build/oaudio
OUT ?= ../../bin
UNAME := $(shell uname -s)
# GGML_NATIVE is off so that a build runs on any CPU of its architecture, not
# only the one it was built on. OpenMP is off as in the other runners.
CMAKE_FLAGS := -DCMAKE_BUILD_TYPE=Release -DGGML_NATIVE=OFF -DGGML_OPENMP=OFF
ifeq ($(UNAME),Darwin)
# Metal shaders embedded as source: the build needs only the Command Line
# Tools, not Xcode's Metal compiler.
CMAKE_FLAGS += -DGGML_METAL=ON -DGGML_METAL_EMBED_LIBRARY=ON
MACOSX_DEPLOYMENT_TARGET ?= 14.0
CMAKE_FLAGS += -DCMAKE_OSX_DEPLOYMENT_TARGET=$(MACOSX_DEPLOYMENT_TARGET)
endif

.PHONY: all clean
all: $(OUT)/speechcpp $(OUT)/oaudio-runner-speech.cpp

$(SRC)/CMakeLists.txt:
	git clone --depth 1 --branch $(SPEECHCPP_TAG) --recurse-submodules --shallow-submodules https://github.com/nyosegawa/speech.cpp.git $(SRC)

$(BUILD)/speech: $(SRC)/CMakeLists.txt
	cmake -S $(SRC) -B $(BUILD) $(CMAKE_FLAGS)
	cmake --build $(BUILD) --config Release --target speech-cli -j

$(OUT)/speechcpp: $(BUILD)/speech
	cp $< $@

$(OUT)/oaudio-runner-speech.cpp: $(filter-out %_test.go,$(wildcard *.go)) go.mod $(wildcard ../../runner/*.go)
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.engineVersion=$(SPEECHCPP_TAG)" -o $@ .

clean:
	rm -rf $(BUILD)

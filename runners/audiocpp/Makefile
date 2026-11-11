# Builds oaudio-runner-audio.cpp and the audio.cpp server it drives, placing
# both in OUT. Needs git, cmake and a C++ compiler; the first build takes a
# few minutes.
#
# Only the model families in AUDIOCPP_MODELS are compiled in. Add a family
# here when a manifest needs it.
AUDIOCPP_TAG ?= v0.9.4
AUDIOCPP_MODELS ?= qwen3_tts,chatterbox
SRC := third_party/audio.cpp
BUILD := $(SRC)/build/oaudio
OUT ?= ../../bin
UNAME := $(shell uname -s)
CMAKE_FLAGS := -DCMAKE_BUILD_TYPE=Release -DENGINE_ENABLE_CUDA=OFF -DENGINE_ENABLE_VULKAN=OFF \
	-DENGINE_BUILD_TESTS=OFF -DENGINE_BUILD_EXAMPLES=OFF -DENGINE_BUILD_WARMBENCH=OFF \
	-DAUDIOCPP_DEPLOYMENT_BUILD=ON -DAUDIOCPP_BUILD_NATIVE_MODEL_MANAGER=OFF \
	-DAUDIOCPP_MODEL_SET=custom "-DAUDIOCPP_MODELS=$(AUDIOCPP_MODELS)"
ifeq ($(UNAME),Darwin)
# Embedding the Metal shaders as source means the build needs only the
# Command Line Tools, not Xcode's Metal compiler. audio.cpp's own
# scripts/build_metal.sh insists on the latter.
# OpenMP is off, as in that script: Apple's compiler does not ship it.
CMAKE_FLAGS += -DENGINE_ENABLE_METAL=ON -DGGML_METAL_EMBED_LIBRARY=ON \
	-DENGINE_ENABLE_OPENMP=OFF -DGGML_OPENMP=OFF
endif

.PHONY: all clean
all: $(OUT)/audiocpp_server $(OUT)/oaudio-runner-audio.cpp

$(SRC)/CMakeLists.txt:
	git clone --depth 1 --branch $(AUDIOCPP_TAG) --recurse-submodules --shallow-submodules https://github.com/0xShug0/audio.cpp.git $(SRC)

$(BUILD)/bin/audiocpp_server: $(SRC)/CMakeLists.txt
	cmake -S $(SRC) -B $(BUILD) $(CMAKE_FLAGS)
	cmake --build $(BUILD) --config Release --target audiocpp_server -j

$(OUT)/audiocpp_server: $(BUILD)/bin/audiocpp_server
	cp $< $@

$(OUT)/oaudio-runner-audio.cpp: main.go
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.engineVersion=$(AUDIOCPP_TAG)" -o $@ .

clean:
	rm -rf $(BUILD)

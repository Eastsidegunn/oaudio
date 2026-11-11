# Builds oaudio-runner-whisper.cpp: whisper.cpp linked statically, with Metal
# on macOS. Needs git, cmake and a C++ compiler.
WHISPER_TAG ?= v1.9.5
SRC := third_party/whisper.cpp
OUT ?= ../../bin
UNAME := $(shell uname -s)
CMAKE_FLAGS := -DBUILD_SHARED_LIBS=OFF -DCMAKE_BUILD_TYPE=Release -DWHISPER_BUILD_TESTS=OFF -DWHISPER_BUILD_EXAMPLES=OFF
LIBDIRS = $(SRC)/build/src $(SRC)/build/ggml/src $(SRC)/build/ggml/src/ggml-metal $(SRC)/build/ggml/src/ggml-blas
ifeq ($(UNAME),Darwin)
CMAKE_FLAGS += -DGGML_METAL=ON -DGGML_METAL_EMBED_LIBRARY=ON
MACOSX_DEPLOYMENT_TARGET ?= 14.0
CMAKE_FLAGS += -DCMAKE_OSX_DEPLOYMENT_TARGET=$(MACOSX_DEPLOYMENT_TARGET)
# Named in the flags, not only the environment, so that Go's build cache
# cannot reuse C objects compiled for another macOS version.
export CGO_CFLAGS ?= -O2 -g -mmacosx-version-min=$(MACOSX_DEPLOYMENT_TARGET)
export CGO_LDFLAGS ?= -mmacosx-version-min=$(MACOSX_DEPLOYMENT_TARGET)
endif

.PHONY: all clean
all: $(OUT)/oaudio-runner-whisper.cpp

$(SRC)/CMakeLists.txt:
	git clone --depth 1 --branch $(WHISPER_TAG) https://github.com/ggml-org/whisper.cpp.git $(SRC)

$(SRC)/build/src/libwhisper.a: $(SRC)/CMakeLists.txt
	cmake -S $(SRC) -B $(SRC)/build $(CMAKE_FLAGS)
	cmake --build $(SRC)/build --config Release -j

$(OUT)/oaudio-runner-whisper.cpp: main.go $(SRC)/build/src/libwhisper.a
	CGO_ENABLED=1 \
	C_INCLUDE_PATH="$(abspath $(SRC)/include):$(abspath $(SRC)/ggml/include)" \
	LIBRARY_PATH="$(subst $(eval) ,:,$(abspath $(LIBDIRS)))" \
	go build -trimpath -ldflags "-s -w -X main.engineVersion=$(WHISPER_TAG)" -o $@ .

clean:
	rm -rf $(SRC)/build

GO ?= go
LIB_NAME := prism-provider

ifeq ($(OS),Windows_NT)
  EXT := dll
else
  UNAME_S := $(shell uname -s)
  ifeq ($(UNAME_S),Darwin)
    EXT := dylib
  else
    EXT := so
  endif
endif

# cgo needs a C toolchain. Prefer one already on PATH, else fall back to the
# portable MinGW-w64 used to bring this repo up (see README).
ifeq ($(shell command -v gcc 2>/dev/null),)
  ifneq ($(wildcard /c/zjj/toolchain/mingw64/bin/gcc.exe),)
    CC ?= /c/zjj/toolchain/mingw64/bin/gcc.exe
  endif
endif
export CC

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null)
# main.version is a var specifically so this injection takes effect; when no
# version can be derived, pass nothing so the compiled-in default stands.
LDFLAGS := $(if $(VERSION),-X main.version=$(VERSION),)

.PHONY: build test check fmt vet clean all

all: build

# The plugin must be built with CGO as a c-shared library: that is the only
# shape internal/pluginhost/loader_windows.go can load.
build:
	CGO_ENABLED=1 $(GO) build -trimpath -buildmode=c-shared \
		-ldflags "-s -w $(LDFLAGS)" -o $(LIB_NAME).$(EXT) .
	rm -f $(LIB_NAME).h

test:
	CGO_ENABLED=1 $(GO) test -count=1 ./...

# Loads the built library through a host-equivalent loader and exercises every
# RPC method. Needs `build` first.
check: build
	cd tools/plugincheck && $(GO) run . $(CURDIR)/$(LIB_NAME).$(EXT)

fmt:
	gofmt -l -w .

vet:
	$(GO) vet ./...

clean:
	rm -f $(LIB_NAME).so $(LIB_NAME).dylib $(LIB_NAME).dll $(LIB_NAME).h

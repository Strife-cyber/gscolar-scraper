# Builds a single portable gscolar binary (no CGo). Output: bin/gscolar(.exe on Windows).
#
# Works from both cmd.exe (Windows make / PowerShell) and POSIX sh (git bash, Linux, macOS):
# the MKDIR/RM recipes branch on whichever shell make is using. CGO_ENABLED is exported
# through make itself, so no shell-specific `VAR=val` (sh) or `set VAR=val &&` (cmd) prefix
# is needed. Equivalent to:
#   CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/gscolar ./cmd/gscolar

GO          ?= go
GOOS        := $(shell $(GO) env GOOS)
BIN_DIR     := bin
BINARY      := $(BIN_DIR)/gscolar$(if $(filter windows,$(GOOS)),.exe,)
PKG         := ./cmd/gscolar
LDFLAGS     := -s -w

export CGO_ENABLED
CGO_ENABLED ?= 0

# cmd.exe is GNU make's default shell on Windows when no POSIX sh is on PATH; its
# mkdir/rmdir syntax differs from sh's. Detect via COMSPEC: cmd expands it, sh prints it literally.
ifeq ($(findstring cmd.exe,$(shell echo %COMSPEC%)),)
  MKDIR := mkdir -p $(BIN_DIR)
  RM    := rm -rf $(BIN_DIR)
else
  MKDIR := if not exist $(BIN_DIR) mkdir $(BIN_DIR)
  RM    := if exist $(BIN_DIR) rmdir /s /q $(BIN_DIR)
endif

.PHONY: all build test clean

all: build

build:
	@$(MKDIR)
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) $(PKG)
	@echo Built $(BINARY)

test:
	$(GO) test ./...

clean:
	@$(RM)

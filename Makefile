APP=spettro
VERSION ?= dev
LDFLAGS := -s -w -X spettro/internal/version.App=$(VERSION)

# Windows will not execute a file without the .exe extension, so the native
# build needs it even when make is driven from Git Bash or MSYS2.
EXE :=
ifeq ($(OS),Windows_NT)
EXE := .exe
endif

.PHONY: test bench build build-all install

test:
	go test ./...

bench:
	go test -bench=. -run=^$$ ./internal/budget

build:
	go build -trimpath -ldflags="$(LDFLAGS)" -o bin/$(APP)$(EXE) ./cmd/spettro

build-all:
	CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/$(APP)-linux-amd64 ./cmd/spettro
	CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/$(APP)-linux-arm64 ./cmd/spettro
	CGO_ENABLED=0 GOOS=darwin  GOARCH=amd64 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/$(APP)-darwin-amd64 ./cmd/spettro
	CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/$(APP)-darwin-arm64 ./cmd/spettro
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/$(APP)-windows-amd64.exe ./cmd/spettro
	CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/$(APP)-windows-arm64.exe ./cmd/spettro

INSTALL_DIR ?= $(HOME)/.local/bin

install: build
	mkdir -p $(INSTALL_DIR)
	# rm first: cp onto an existing inode leaves the kernel's cached code
	# signature stale on macOS, and the binary gets SIGKILLed at launch.
	# On Windows it is also what lets the copy replace a build that is
	# currently running, which cannot be overwritten in place.
	rm -f $(INSTALL_DIR)/$(APP)$(EXE)
	cp bin/$(APP)$(EXE) $(INSTALL_DIR)/$(APP)$(EXE)

APP=spettro
VERSION ?= dev
# Every packaged binary is built with GOFLAGS_RELEASE and LDFLAGS: -trimpath
# drops local paths, -s -w drop the symbol table and DWARF data (79 MB to
# 55 MB measured on darwin/arm64, and less to page in at every start).
GOFLAGS_RELEASE := -trimpath
LDFLAGS := -s -w -X spettro/internal/version.App=$(VERSION)

# Windows will not execute a file without the .exe extension, so the native
# build needs it even when make is driven from Git Bash or MSYS2.
EXE :=
ifeq ($(OS),Windows_NT)
EXE := .exe
endif

.PHONY: test bench build build-all install size

test:
	go test ./...

bench:
	go test -bench=. -run=^$$ ./internal/budget

build:
	go build $(GOFLAGS_RELEASE) -ldflags="$(LDFLAGS)" -o bin/$(APP)$(EXE) ./cmd/spettro

build-all:
	CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build $(GOFLAGS_RELEASE) -ldflags="$(LDFLAGS)" -o bin/$(APP)-linux-amd64 ./cmd/spettro
	CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build $(GOFLAGS_RELEASE) -ldflags="$(LDFLAGS)" -o bin/$(APP)-linux-arm64 ./cmd/spettro
	CGO_ENABLED=0 GOOS=darwin  GOARCH=amd64 go build $(GOFLAGS_RELEASE) -ldflags="$(LDFLAGS)" -o bin/$(APP)-darwin-amd64 ./cmd/spettro
	CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build $(GOFLAGS_RELEASE) -ldflags="$(LDFLAGS)" -o bin/$(APP)-darwin-arm64 ./cmd/spettro
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build $(GOFLAGS_RELEASE) -ldflags="$(LDFLAGS)" -o bin/$(APP)-windows-amd64.exe ./cmd/spettro
	CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build $(GOFLAGS_RELEASE) -ldflags="$(LDFLAGS)" -o bin/$(APP)-windows-arm64.exe ./cmd/spettro

# size prints the release binary's size and the number of packages linked
# into it, so a dependency that bloats either shows up in review.
size: build
	@wc -c < bin/$(APP)$(EXE) | awk '{ printf "binary:   %.1f MB (%d bytes)\n", $$1 / 1048576, $$1 }'
	@go list -deps ./cmd/spettro | wc -l | awk '{ printf "packages: %d\n", $$1 }'

INSTALL_DIR ?= $(HOME)/.local/bin

install: build
	mkdir -p $(INSTALL_DIR)
	# rm first: cp onto an existing inode leaves the kernel's cached code
	# signature stale on macOS, and the binary gets SIGKILLed at launch.
	# On Windows it is also what lets the copy replace a build that is
	# currently running, which cannot be overwritten in place.
	rm -f $(INSTALL_DIR)/$(APP)$(EXE)
	cp bin/$(APP)$(EXE) $(INSTALL_DIR)/$(APP)$(EXE)

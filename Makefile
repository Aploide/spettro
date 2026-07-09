APP=spettro
VERSION ?= dev
LDFLAGS := -s -w -X spettro/internal/version.App=$(VERSION)

.PHONY: test bench build build-all install

test:
	go test ./...

bench:
	go test -bench=. -run=^$$ ./internal/budget

build:
	go build -trimpath -ldflags="$(LDFLAGS)" -o bin/$(APP) ./cmd/spettro

build-all:
	CGO_ENABLED=0 GOOS=linux  GOARCH=amd64 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/$(APP)-linux-amd64 ./cmd/spettro
	CGO_ENABLED=0 GOOS=linux  GOARCH=arm64 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/$(APP)-linux-arm64 ./cmd/spettro
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/$(APP)-darwin-amd64 ./cmd/spettro
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/$(APP)-darwin-arm64 ./cmd/spettro

INSTALL_DIR ?= $(HOME)/.local/bin

install: build
	mkdir -p $(INSTALL_DIR)
	# rm first: cp onto an existing inode leaves the kernel's cached code
	# signature stale on macOS, and the binary gets SIGKILLed at launch
	rm -f $(INSTALL_DIR)/$(APP)
	cp bin/$(APP) $(INSTALL_DIR)/$(APP)

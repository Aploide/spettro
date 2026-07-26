APP=spettro
VERSION ?= dev
LDFLAGS := -s -w -X spettro/internal/version.App=$(VERSION)

.PHONY: test bench build build-all install ios-framework ios-tools ios-check

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

# --- iOS -----------------------------------------------------------------
#
# The engine ships inside the app on iOS: apps cannot execute a subprocess, so
# spettro/mobile is bound into SpettroKit.xcframework and driven in-process
# over ACP. The artifact is built, never committed — the app repo gitignores
# it, and the engine version is therefore whatever was bound at app build time.
#
# IOS_MIN must match (or be below) the app's IPHONEOS_DEPLOYMENT_TARGET.
IOS_MIN ?= 26.0
IOS_APP_REPO ?= $(abspath $(CURDIR)/../Spettro)
IOS_FRAMEWORK_DIR ?= $(IOS_APP_REPO)/Frameworks
IOS_FRAMEWORK := $(IOS_FRAMEWORK_DIR)/SpettroKit.xcframework
TOOLS_DIR := $(CURDIR)/bin/tools

# gomobile shells out to `gobind`, which it can only find on PATH. Build both
# from the versions pinned by this module's `tool` directives rather than
# `gomobile init` (which would `go install ...@latest` and silently float the
# binding generator away from the pinned library).
ios-tools:
	mkdir -p $(TOOLS_DIR)
	go build -o $(TOOLS_DIR)/gobind golang.org/x/mobile/cmd/gobind
	go build -o $(TOOLS_DIR)/gomobile golang.org/x/mobile/cmd/gomobile

ios-framework: ios-tools
	mkdir -p $(IOS_FRAMEWORK_DIR)
	# gomobile merges into an existing xcframework rather than replacing it;
	# start clean so a removed slice cannot survive a rebuild.
	rm -rf $(IOS_FRAMEWORK)
	PATH="$(TOOLS_DIR):$$PATH" $(TOOLS_DIR)/gomobile bind \
		-target ios,iossimulator \
		-iosversion $(IOS_MIN) \
		-trimpath \
		-ldflags="-X spettro/internal/version.App=$(VERSION)" \
		-o $(IOS_FRAMEWORK) \
		spettro/mobile
	@echo "built $(IOS_FRAMEWORK)"

# Compile-only check of the iOS surface, device + simulator. CGO_LDFLAGS is not
# optional: GOOS=ios always links externally, and the linker gets none of the
# -isysroot/-arch from CGO_CFLAGS (symptom: "ld: library 'resolv' not found").
ios-check:
	GOOS=ios GOARCH=arm64 CGO_ENABLED=1 \
		CC="$$(xcrun --sdk iphoneos -f clang)" \
		CGO_CFLAGS="-isysroot $$(xcrun --sdk iphoneos --show-sdk-path) -miphoneos-version-min=$(IOS_MIN) -arch arm64" \
		CGO_LDFLAGS="-isysroot $$(xcrun --sdk iphoneos --show-sdk-path) -miphoneos-version-min=$(IOS_MIN) -arch arm64" \
		go build ./...
	GOOS=ios GOARCH=arm64 CGO_ENABLED=1 \
		CC="$$(xcrun --sdk iphonesimulator -f clang)" \
		CGO_CFLAGS="-isysroot $$(xcrun --sdk iphonesimulator --show-sdk-path) -mios-simulator-version-min=$(IOS_MIN) -arch arm64" \
		CGO_LDFLAGS="-isysroot $$(xcrun --sdk iphonesimulator --show-sdk-path) -mios-simulator-version-min=$(IOS_MIN) -arch arm64" \
		go build ./...

INSTALL_DIR ?= $(HOME)/.local/bin

install: build
	mkdir -p $(INSTALL_DIR)
	# rm first: cp onto an existing inode leaves the kernel's cached code
	# signature stale on macOS, and the binary gets SIGKILLed at launch
	rm -f $(INSTALL_DIR)/$(APP)
	cp bin/$(APP) $(INSTALL_DIR)/$(APP)

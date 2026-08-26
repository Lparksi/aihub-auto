PLUGIN_NAME := aihub-auto
ENTRYPOINT := ./cmd/aihub-auto-plugin
DIST_DIR := dist
HOST_GOOS := $(shell go env GOOS)

.PHONY: build build-darwin build-linux build-windows test vet clean

build:
	@case "$(HOST_GOOS)" in \
		darwin) $(MAKE) build-darwin ;; \
		linux) $(MAKE) build-linux ;; \
		windows) $(MAKE) build-windows ;; \
		*) echo "Unsupported native build platform: $(HOST_GOOS)"; exit 1 ;; \
	esac

build-darwin:
	@test "$(HOST_GOOS)" = "darwin" || { echo "build-darwin must run on macOS with its native C toolchain; cross-compiling c-shared is unsupported."; exit 1; }
	mkdir -p $(DIST_DIR)
	CGO_ENABLED=1 go build -buildmode=c-shared -o $(DIST_DIR)/$(PLUGIN_NAME).dylib $(ENTRYPOINT)

build-linux:
	@test "$(HOST_GOOS)" = "linux" || { echo "build-linux must run on Linux with its native C toolchain; cross-compiling c-shared is unsupported."; exit 1; }
	mkdir -p $(DIST_DIR)
	CGO_ENABLED=1 go build -buildmode=c-shared -o $(DIST_DIR)/$(PLUGIN_NAME).so $(ENTRYPOINT)

build-windows:
	@test "$(HOST_GOOS)" = "windows" || { echo "build-windows must run on Windows with its native C toolchain; cross-compiling c-shared is unsupported."; exit 1; }
	mkdir -p $(DIST_DIR)
	CGO_ENABLED=1 go build -buildmode=c-shared -o $(DIST_DIR)/$(PLUGIN_NAME).dll $(ENTRYPOINT)

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -rf $(DIST_DIR)

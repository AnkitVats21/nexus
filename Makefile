.PHONY: all build build-hub build-hub-pi run-hub build-stub build-arm64 build-armhf build-pi test clean run run-local run-local-2 run-standalone run-stub

BINARY_NAME=bin/orbit
STUB_NAME=bin/nexus-stub

all: build build-hub build-stub

build:
	@mkdir -p bin
	go build -o $(BINARY_NAME) ./cmd/orbit

# The board's hub (pkg/hub). WEB: the dashboard's device build
# (nexus-dashboard: npx vite build --mode device).
WEB ?= ../nexus-dashboard/dist

build-hub:
	@mkdir -p bin
	go build -o bin/nexus-hub ./cmd/nexus-hub

build-hub-pi:
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o bin/nexus-hub-linux-arm64 ./cmd/nexus-hub

run-hub: build-hub
	./bin/nexus-hub --web $(WEB)

build-stub:
	@mkdir -p bin
	go build -o $(STUB_NAME) ./cmd/nexus-stub

build-arm64:
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o bin/orbit-linux-arm64 ./cmd/orbit

build-armhf:
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -ldflags="-s -w" -o bin/orbit-linux-armhf ./cmd/orbit

build-pi: build-arm64 build-armhf

test:
	go test -v ./...

run: build
	./$(BINARY_NAME)

run-local: build
	./$(BINARY_NAME) --nexus-host localhost

run-local-2: build
	./$(BINARY_NAME) --nexus-host localhost --satellite-id satellite-secondary --satellite-name "Secondary Satellite" --mpv-socket /tmp/orbit-sat2.sock

run-standalone: build
	./$(BINARY_NAME) --standalone

run-stub: build-stub
	./$(STUB_NAME)

clean:
	rm -rf bin/

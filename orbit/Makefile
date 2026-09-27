.PHONY: all build build-stub test clean run run-standalone run-stub

BINARY_NAME=bin/orbit
STUB_NAME=bin/nexus-stub

all: build build-stub

build:
	@mkdir -p bin
	go build -o $(BINARY_NAME) ./cmd/orbit

build-stub:
	@mkdir -p bin
	go build -o $(STUB_NAME) ./cmd/nexus-stub

test:
	go test -v ./...

run: build
	./$(BINARY_NAME)

run-standalone: build
	./$(BINARY_NAME) --standalone

run-stub: build-stub
	./$(STUB_NAME)

clean:
	rm -rf bin/

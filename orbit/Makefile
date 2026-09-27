.PHONY: all build test clean run run-standalone

BINARY_NAME=bin/orbit

all: build

build:
	@mkdir -p bin
	go build -o $(BINARY_NAME) ./cmd/orbit

test:
	go test -v ./...

run: build
	./$(BINARY_NAME)

run-standalone: build
	./$(BINARY_NAME) --standalone

clean:
	rm -rf bin/

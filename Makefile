.PHONY: all build test verify clean

all: verify

build:
	mkdir -p bin
	go build -trimpath -o bin/01agent ./cmd/01agent

test:
	go test ./...

verify:
	test -z "$$(gofmt -l cmd internal)"
	go vet ./...
	go test -race -coverprofile=coverage.out ./...
	go build ./cmd/01agent

clean:
	rm -f bin/01agent coverage.out

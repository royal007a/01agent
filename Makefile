.PHONY: all build test verify clean

all: verify

build:
	mkdir -p bin
	go build -trimpath -o bin/01agent ./cmd/01agent
	go build -trimpath -o bin/01agentd ./cmd/01agentd

test:
	go test ./...

verify:
	test -z "$$(gofmt -l cmd internal)"
	go vet ./...
	go test -race -coverprofile=coverage.out ./...
	mkdir -p bin
	go build -o bin/01agent ./cmd/01agent
	go build -o bin/01agentd ./cmd/01agentd

clean:
	rm -f bin/01agent bin/01agentd coverage.out

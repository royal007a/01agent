.PHONY: all build test eval verify clean

all: verify

build:
	mkdir -p bin
	go build -trimpath -o bin/01agent ./cmd/01agent
	go build -trimpath -o bin/01agentd ./cmd/01agentd
	go build -trimpath -o bin/01agent-eval ./cmd/01agent-eval
	go build -trimpath -o bin/01agent-replay ./cmd/01agent-replay

test:
	go test ./...

eval:
	mkdir -p artifacts/eval
	go run ./cmd/01agent-eval --suite evals/runtime.json --artifacts artifacts/eval --report artifacts/eval/report.json

verify:
	test -z "$$(gofmt -l cmd internal)"
	go vet ./...
	go test -race -coverprofile=coverage.out ./...
	mkdir -p bin
	go build -o bin/01agent ./cmd/01agent
	go build -o bin/01agentd ./cmd/01agentd
	go build -o bin/01agent-eval ./cmd/01agent-eval
	go build -o bin/01agent-replay ./cmd/01agent-replay
	$(MAKE) eval

clean:
	rm -f bin/01agent bin/01agentd bin/01agent-eval bin/01agent-replay coverage.out

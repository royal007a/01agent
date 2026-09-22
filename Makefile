.PHONY: all build test eval verify clean

all: verify

build:
	mkdir -p bin
	go build -trimpath -o bin/01agent ./cmd/01agent
	go build -trimpath -o bin/01agentd ./cmd/01agentd
	go build -trimpath -o bin/01agent-feishu ./cmd/01agent-feishu
	go build -trimpath -o bin/01agent-eval ./cmd/01agent-eval
	go build -trimpath -o bin/01agent-control-eval ./cmd/01agent-control-eval
	go build -trimpath -o bin/01agent-daemon ./cmd/01agent-daemon
	go build -trimpath -o bin/01agent-replay ./cmd/01agent-replay
	GOOS=linux go build -trimpath -o bin/01agent-sandbox ./cmd/01agent-sandbox

test:
	go test ./...

eval:
	mkdir -p artifacts/eval
	go run ./cmd/01agent-eval --suite evals/runtime.json --artifacts artifacts/eval --report artifacts/eval/report.json
	go run ./cmd/01agent-control-eval --suite evals/control-plane.json --report artifacts/eval/control-plane-report.json

verify:
	test -z "$$(gofmt -l cmd internal)"
	go vet ./...
	go test -race -coverprofile=coverage.out ./...
	mkdir -p bin
	go build -o bin/01agent ./cmd/01agent
	go build -o bin/01agentd ./cmd/01agentd
	go build -o bin/01agent-feishu ./cmd/01agent-feishu
	go build -o bin/01agent-eval ./cmd/01agent-eval
	go build -o bin/01agent-control-eval ./cmd/01agent-control-eval
	go build -o bin/01agent-daemon ./cmd/01agent-daemon
	go build -o bin/01agent-replay ./cmd/01agent-replay
	GOOS=linux go build -o bin/01agent-sandbox ./cmd/01agent-sandbox
	$(MAKE) eval

clean:
	rm -f bin/01agent bin/01agentd bin/01agent-feishu bin/01agent-eval bin/01agent-control-eval bin/01agent-daemon bin/01agent-replay bin/01agent-sandbox coverage.out

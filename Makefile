BIN      ?= $(HOME)/.local/bin/claw
PLIST    ?= $(HOME)/Library/LaunchAgents/com.weberzhao.go-tiny-claw.plist
LABEL    := com.weberzhao.go-tiny-claw

.PHONY: build test live install deploy undeploy status logs

build:
	go build -o claw ./cmd/claw

test:
	go vet ./... && go test ./...

# 真实模型逐讲复现：make live RUN=L12  PROTO=openai|claude
live:
	bash -c 'source scripts/env.sh $(or $(PROTO),openai) && go test -tags live ./internal/e2e -v -timeout 90m -run "$(or $(RUN),L)"'

install:
	go build -trimpath -o $(BIN) ./cmd/claw
	@echo "installed $(BIN)"

# 本地部署为 launchd 常驻服务（127.0.0.1:48080）
deploy: install
	mkdir -p $(HOME)/.go-tiny-claw/workspace $(HOME)/.go-tiny-claw/logs
	sed -e "s#__BIN__#$(BIN)#g" -e "s#__REPO__#$(CURDIR)#g" -e "s#__HOME__#$(HOME)#g" deploy/launchd.plist.tmpl > $(PLIST)
	-launchctl bootout gui/$$(id -u)/$(LABEL) 2>/dev/null
	@for i in 1 2 3 4 5; do launchctl print gui/$$(id -u)/$(LABEL) >/dev/null 2>&1 || break; sleep 1; done
	launchctl bootstrap gui/$$(id -u) $(PLIST)
	@sleep 2; curl -fsS http://127.0.0.1:48080/healthz && echo "deployed: http://127.0.0.1:48080"

undeploy:
	-launchctl bootout gui/$$(id -u)/$(LABEL)
	rm -f $(PLIST)

status:
	launchctl print gui/$$(id -u)/$(LABEL) | head -20; curl -fsS http://127.0.0.1:48080/metrics

logs:
	tail -f $(HOME)/.go-tiny-claw/logs/claw.log

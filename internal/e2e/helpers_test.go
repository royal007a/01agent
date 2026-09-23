//go:build live

// Package e2e 用真实大模型复现课程每一讲的"奇迹时刻"。
//
//	source scripts/env.sh openai && go test -tags live ./internal/e2e -v -run L12
package e2e

import (
	"bytes"
	"context"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/weberzhao/go-tiny-claw/internal/engine"
	"github.com/weberzhao/go-tiny-claw/internal/provider"
	"github.com/weberzhao/go-tiny-claw/internal/tools"
)

// logBuf 同时把日志写到终端与内存，便于断言引擎内部行为（如 Compactor 是否触发）。
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	os.Stderr.Write(p)
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func captureLog(t *testing.T) *logBuf {
	lb := &logBuf{}
	log.SetOutput(lb)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return lb
}

func realProvider(t *testing.T) provider.LLMProvider {
	cfg := provider.ConfigFromEnv()
	if os.Getenv("CLAW_API_KEY") == "" {
		t.Skip("未设置 CLAW_API_KEY，跳过真实模型测试（source scripts/env.sh）")
	}
	p, err := provider.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func fullRegistry(wd string) tools.Registry {
	r := tools.NewRegistry()
	r.Register(tools.NewReadFileTool(wd))
	r.Register(tools.NewWriteFileTool(wd))
	r.Register(tools.NewBashTool(wd))
	r.Register(tools.NewEditFileTool(wd))
	return r
}

func workspace(t *testing.T) string {
	wd := filepath.Join(t.TempDir(), "workspace")
	os.MkdirAll(wd, 0o755)
	return wd
}

func write(t *testing.T, path, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sh(t *testing.T, dir string, cmd string) string {
	t.Helper()
	c := exec.Command("bash", "-c", cmd)
	c.Dir = dir
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", cmd, err, out)
	}
	return string(out)
}

func ctxTimeout(t *testing.T) context.Context {
	d := 20 * time.Minute
	if v, err := time.ParseDuration(os.Getenv("CLAW_E2E_TIMEOUT")); err == nil {
		d = v
	}
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// recReporter 记录 Reporter 事件，便于断言工具调用轨迹。
type recReporter struct {
	engine.TerminalReporter
	mu     sync.Mutex
	calls  []string
	msgs   []string
	onCall func(count int)
}

func newRec() *recReporter {
	return &recReporter{TerminalReporter: engine.TerminalReporter{Out: os.Stdout}}
}

func (r *recReporter) OnToolCall(ctx context.Context, name, args string) {
	r.mu.Lock()
	r.calls = append(r.calls, name+" "+args)
	n, cb := len(r.calls), r.onCall
	r.mu.Unlock()
	if cb != nil {
		cb(n)
	}
	r.TerminalReporter.OnToolCall(ctx, name, args)
}

func (r *recReporter) OnMessage(ctx context.Context, c string) {
	r.mu.Lock()
	r.msgs = append(r.msgs, c)
	r.mu.Unlock()
	r.TerminalReporter.OnMessage(ctx, c)
}

func (r *recReporter) callsJoined() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.calls, "\n")
}

func (r *recReporter) lastMsg() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.msgs) == 0 {
		return ""
	}
	return r.msgs[len(r.msgs)-1]
}

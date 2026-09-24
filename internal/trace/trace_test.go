package trace

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTreeAndExport(t *testing.T) {
	ctx, root := Start(context.Background(), "Agent.Run")
	root.Set("SessionID", "s/1")
	turnCtx, turn := Start(ctx, "Turn-1")
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, sp := Start(turnCtx, "Tool.Execute")
			sp.Set("i", i)
			time.Sleep(20 * time.Millisecond)
			sp.End()
			turn.Add("tool_calls", 1)
		}(i)
	}
	wg.Wait()
	turn.End()
	root.End()
	root.End() // 幂等
	if root.Count("Tool.Execute") != 3 || turn.Get("tool_calls") != 3 {
		t.Fatal("树结构或计数错误")
	}
	if turn.DurationMs < 20 || turn.DurationMs > 200 {
		t.Fatalf("turn 耗时异常 %d（并发应约 20ms）", turn.DurationMs)
	}
	dir := t.TempDir()
	path, err := Export(root, dir, "s/1")
	if err != nil || !strings.HasPrefix(filepath.Base(path), "trace_s_1_") {
		t.Fatal(path, err)
	}
	var back map[string]any
	b, _ := os.ReadFile(path)
	if json.Unmarshal(b, &back) != nil || back["name"] != "Agent.Run" {
		t.Fatal("决策树 JSON 无效")
	}
	pb, _ := os.ReadFile(strings.TrimSuffix(path, ".json") + ".perfetto.json")
	var pf struct {
		TraceEvents []map[string]any `json:"traceEvents"`
	}
	if json.Unmarshal(pb, &pf) != nil || len(pf.TraceEvents) != 5 {
		t.Fatalf("perfetto 事件数错误: %d", len(pf.TraceEvents))
	}
	tids := map[float64]bool{}
	for _, e := range pf.TraceEvents {
		if e["name"] == "Tool.Execute" {
			tids[e["tid"].(float64)] = true
		}
	}
	if len(tids) != 3 {
		t.Fatalf("并发工具应分到 3 条轨道，got %d", len(tids))
	}
	var nilSpan *Span
	nilSpan.Set("x", 1)
	nilSpan.End()
}

func TestExportNoOverwrite(t *testing.T) {
	_, root := Start(context.Background(), "Agent.Run")
	root.End()
	dir := t.TempDir()
	seen := map[string]bool{}
	for i := 0; i < 20; i++ { // 同一个根 Span（同会话同毫秒）重复导出
		p, err := Export(root, dir, "s")
		if err != nil || seen[p] {
			t.Fatalf("覆盖或失败: %v %s", err, p)
		}
		seen[p] = true
	}
	if files, _ := filepath.Glob(filepath.Join(dir, "*.perfetto.json")); len(files) != 20 {
		t.Fatalf("perfetto 文件数 %d", len(files))
	}
	ro := filepath.Join(dir, "ro")
	os.Mkdir(ro, 0o500)
	if _, err := Export(root, ro, "s"); err == nil {
		t.Fatal("不可写目录应返回错误而不是静默丢弃")
	}
}

// claw-bench：第 20 讲的自动化跑分入口。
//
//	source scripts/env.sh openai
//	go run ./cmd/bench -cases bench/cases.json -j 3 -out bench/reports
//	go run ./cmd/bench -baseline bench/reports/latest.json   # 与基线对比，出现回归则非零退出（可接 CI）
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/weberzhao/go-tiny-claw/internal/eval"
	"github.com/weberzhao/go-tiny-claw/internal/provider"
)

func main() {
	casesPath := flag.String("cases", "bench/cases.json", "评测集 JSON")
	filter := flag.String("run", "", "只运行 ID 匹配该正则的用例")
	par := flag.Int("j", 2, "并发执行的用例数")
	outDir := flag.String("out", "bench/reports", "报告输出目录")
	baselinePath := flag.String("baseline", "", "基线报告 JSON（用于对比与回归检测）")
	workRoot := flag.String("workroot", filepath.Join(os.TempDir(), "claw-bench"), "用例工作区根目录")
	keep := flag.Bool("keep", false, "保留通过用例的工作区")
	verbose := flag.Bool("v", false, "显示引擎详细日志")
	flag.Parse()

	cases, err := eval.LoadCases(*casesPath, *filter)
	if err != nil || len(cases) == 0 {
		log.Fatalf("没有可运行的用例: %v", err)
	}
	cfg := provider.ConfigFromEnv()
	if cfg.APIKey == "" {
		log.Fatal("请先配置模型：source scripts/env.sh openai")
	}
	// 屏蔽引擎的逐轮日志，只保留"裁判视角"
	logOut := io.Writer(os.Stderr)
	if !*verbose {
		logOut = &filterWriter{w: os.Stderr}
	}
	log.SetOutput(logOut)
	log.Printf("🚀 启动自动化 Harness Benchmark | 模型 %s（%s）| 用例 %d 个 | 并发 %d", cfg.Model, cfg.Kind, len(cases), *par)

	r := &eval.Runner{Provider: cfg, WorkRoot: *workRoot, Parallelism: *par, KeepPassed: *keep}
	rep := r.RunSuite(context.Background(), cases)

	var baseline *eval.Report
	if *baselinePath != "" {
		if b, err := os.ReadFile(*baselinePath); err == nil {
			baseline = &eval.Report{}
			if err := json.Unmarshal(b, baseline); err != nil {
				log.Printf("基线解析失败，忽略: %v", err)
				baseline = nil
			}
		}
	}
	md := rep.Markdown(baseline)
	fmt.Println(md)

	_ = os.MkdirAll(*outDir, 0o755)
	stamp := rep.StartedAt.Format("20060102-150405")
	js, _ := json.MarshalIndent(rep, "", "  ")
	for _, name := range []string{"report-" + stamp + ".json", "latest.json"} {
		_ = os.WriteFile(filepath.Join(*outDir, name), js, 0o644)
	}
	_ = os.WriteFile(filepath.Join(*outDir, "report-"+stamp+".md"), []byte(md), 0o644)
	log.Printf("📄 报告已写入 %s/report-%s.{json,md}", *outDir, stamp)

	if baseline != nil {
		if reg := rep.Regressions(*baseline); len(reg) > 0 {
			log.Printf("⚠️ 相对基线出现回归: %v", reg)
			os.Exit(2)
		}
	}
	if rep.Passed < rep.Total {
		os.Exit(1)
	}
}

// filterWriter 只放行 Benchmark 自己的进度行（>>> / 🚀 / 📄 / ⚠️）。
type filterWriter struct{ w io.Writer }

func (f *filterWriter) Write(p []byte) (int, error) {
	s := string(p)
	for _, k := range []string{">>>", "🚀", "📄", "⚠️ 相对基线", "基线解析"} {
		if contains(s, k) {
			return f.w.Write(p)
		}
	}
	return len(p), nil
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

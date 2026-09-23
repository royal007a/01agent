// go-tiny-claw：一个从 0 构建的 Agent Harness。
//
//	claw -prompt "任务" [-thinking] [-workdir dir]   终端单次运行
//	claw serve [-addr 127.0.0.1:48080]              飞书机器人服务端
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"path/filepath"

	"github.com/weberzhao/go-tiny-claw/internal/engine"
	"github.com/weberzhao/go-tiny-claw/internal/provider"
	"github.com/weberzhao/go-tiny-claw/internal/tools"
)

func buildEngine(workDir string, thinking bool) (*engine.AgentEngine, string) {
	p, err := provider.New(provider.ConfigFromEnv())
	if err != nil {
		log.Fatal(err)
	}
	wd, _ := filepath.Abs(workDir)
	if err := os.MkdirAll(wd, 0o755); err != nil {
		log.Fatal(err)
	}
	registry := tools.NewRegistry()
	registry.Register(tools.NewReadFileTool(wd))
	registry.Register(tools.NewWriteFileTool(wd))
	registry.Register(tools.NewBashTool(wd))
	registry.Register(tools.NewEditFileTool(wd))
	return engine.NewAgentEngine(p, registry, wd, thinking), wd
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		serve(os.Args[2:])
		return
	}
	prompt := flag.String("prompt", "", "交给 Agent 的任务")
	thinking := flag.Bool("thinking", false, "开启慢思考")
	workDir := flag.String("workdir", ".", "工作区目录")
	flag.Parse()
	if *prompt == "" {
		log.Fatal("用法: claw -prompt \"任务\" [-thinking] [-workdir dir] | claw serve")
	}
	eng, _ := buildEngine(*workDir, *thinking)
	if err := eng.Run(context.Background(), *prompt, engine.NewTerminalReporter()); err != nil {
		log.Fatalf("引擎运行崩溃: %v", err)
	}
}

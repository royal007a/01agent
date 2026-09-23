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

func main() {
	prompt := flag.String("prompt", "", "交给 Agent 的任务")
	thinking := flag.Bool("thinking", false, "开启慢思考")
	workDir := flag.String("workdir", ".", "工作区目录")
	flag.Parse()
	if *prompt == "" {
		log.Fatal("用法: claw -prompt \"任务\" [-thinking] [-workdir dir]")
	}
	p, err := provider.New(provider.ConfigFromEnv())
	if err != nil {
		log.Fatal(err)
	}
	wd, _ := filepath.Abs(*workDir)
	_ = os.MkdirAll(wd, 0o755)

	registry := tools.NewRegistry()
	registry.Register(tools.NewReadFileTool(wd))
	registry.Register(tools.NewWriteFileTool(wd))
	registry.Register(tools.NewBashTool(wd))
	registry.Register(tools.NewEditFileTool(wd))

	eng := engine.NewAgentEngine(p, registry, wd, *thinking)
	if err := eng.Run(context.Background(), *prompt); err != nil {
		log.Fatalf("引擎运行崩溃: %v", err)
	}
}

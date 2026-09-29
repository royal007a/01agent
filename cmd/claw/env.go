package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/weberzhao/go-tiny-claw/internal/envlock"
	"github.com/weberzhao/go-tiny-claw/internal/provider"
)

// envCmd 环境锁定：
//
//	claw env lock  [-dir wd] [-mode warn|strict] [-probe] [-tools bash,git,...]   拍快照写入 .claw/env.lock.json
//	claw env check [-dir wd] [-probe]                                              与锁比对，有错误级漂移时退出码 1
//	claw env show  [-dir wd]                                                       打印当前环境快照
func envCmd(args []string) {
	if len(args) == 0 {
		log.Fatal("用法: claw env lock|check|show [-dir wd]")
	}
	sub := args[0]
	fs := flag.NewFlagSet("env "+sub, flag.ExitOnError)
	dir := fs.String("dir", ".", "工作区目录")
	mode := fs.String("mode", "", "锁定模式 warn|strict（lock 时生效，默认沿用旧锁或 warn）")
	probe := fs.Bool("probe", false, "发一条极短请求获取服务端回报的实际模型版本")
	toolsFlag := fs.String("tools", "", "要锁定的外部命令，逗号分隔（默认 "+strings.Join(envlock.DefaultTools, ",")+"）")
	_ = fs.Parse(args[1:])
	wd, err := filepath.Abs(*dir)
	if err != nil {
		log.Fatal(err)
	}
	cfg := provider.ConfigFromEnv()
	old, oldErr := envlock.Load(wd)
	if oldErr != nil && !errors.Is(oldErr, envlock.ErrNoLock) && sub != "lock" {
		log.Fatal(oldErr)
	}
	var tools []string
	if *toolsFlag != "" {
		tools = strings.Split(*toolsFlag, ",")
	} else if old != nil {
		tools = old.ToolNames()
	}
	cur, err := envlock.Capture(envlock.CaptureOptions{WorkDir: wd, Model: envlock.ModelOf(cfg), Tools: tools})
	if err != nil {
		log.Fatal(err)
	}
	if *probe {
		p, err := provider.New(cfg)
		if err != nil {
			log.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if cur.Model.Observed, err = envlock.Probe(ctx, p); err != nil {
			log.Fatalf("探测模型版本失败: %v", err)
		}
	}
	switch sub {
	case "show":
		b, _ := json.MarshalIndent(cur, "", "  ")
		fmt.Println(string(b))
	case "lock":
		cur.Mode = envlock.ModeWarn
		if old != nil {
			cur.Mode = old.Mode
			m := cur.Model
			m.Observed = old.Model.Observed
			if !*probe && m == old.Model {
				cur.Model.Observed = old.Model.Observed // 模型配置未变且没重新探测：保留已知的实际版本
			}
		}
		if *mode != "" {
			cur.Mode = envlock.Mode(*mode)
		}
		if cur.Mode != envlock.ModeWarn && cur.Mode != envlock.ModeStrict {
			log.Fatalf("非法 mode %q（warn|strict）", cur.Mode)
		}
		if old != nil {
			for _, d := range envlock.Diff(old, cur) {
				fmt.Println("更新锁定:", d)
			}
		}
		if err := envlock.Save(wd, cur); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("🔒 已锁定环境 → %s（mode=%s，%d 个依赖，%d 个工具，%d 个 Skill 文件，模型 %s/%s）\n",
			filepath.Join(wd, envlock.LockFile), cur.Mode, len(cur.Deps), len(cur.Runtime.Tools), len(cur.Skills), cur.Model.Requested, orDash(cur.Model.Observed))
	case "check":
		if old == nil {
			fmt.Printf("工作区 %s 尚未锁定，先运行 claw env lock\n", wd)
			os.Exit(2)
		}
		ds := envlock.Diff(old, cur)
		if len(ds) == 0 {
			fmt.Printf("✅ 环境与锁一致（%s，mode=%s）\n", envlock.LockFile, old.Mode)
			return
		}
		for _, d := range ds {
			fmt.Println(d)
		}
		if envlock.HasErrors(ds) {
			fmt.Printf("❌ %d 处环境漂移；确认是有意升级后运行 claw env lock 重新锁定\n", len(ds))
			os.Exit(1)
		}
		fmt.Printf("⚠️ %d 处警告级漂移\n", len(ds))
	default:
		log.Fatalf("未知子命令 %q（lock|check|show）", sub)
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

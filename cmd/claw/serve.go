package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/core/httpserverext"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	ctxpkg "github.com/weberzhao/go-tiny-claw/internal/context"
	"github.com/weberzhao/go-tiny-claw/internal/engine"
	"github.com/weberzhao/go-tiny-claw/internal/feishu"
	"github.com/weberzhao/go-tiny-claw/internal/permission"
)

// serve 以飞书机器人服务端方式运行（第 09 讲）。
//
//	FEISHU_APP_ID / FEISHU_APP_SECRET        必填
//	FEISHU_CONNECTION_MODE=ws|webhook        默认 ws：长连接，本地无公网 IP 也能收事件
//	FEISHU_VERIFY_TOKEN / FEISHU_ENCRYPT_KEY webhook 模式下使用
func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:48080", "HTTP 监听地址（webhook 与 /healthz）")
	thinking := fs.Bool("thinking", false, "开启慢思考")
	planMode := fs.Bool("plan", false, "开启 Plan Mode")
	workDir := fs.String("workdir", "./workspace", "工作区目录")
	fs.Parse(args)

	appID, appSecret := os.Getenv("FEISHU_APP_ID"), os.Getenv("FEISHU_APP_SECRET")
	if appID == "" || appSecret == "" {
		log.Fatal("请设置 FEISHU_APP_ID 和 FEISHU_APP_SECRET")
	}
	a := buildEngine(*workDir, *thinking, *planMode)
	// 每个飞书会话（chat_id）一个 Session，JSONL 持久化，重启后自动恢复（第 11 讲）
	sessions := ctxpkg.NewSessionManager(filepath.Join(defaultStateDir(), "sessions"))

	bot := feishu.NewFeishuBot(feishu.NewLarkSender(appID, appSecret),
		func(ctx context.Context, chatID, prompt string, r engine.Reporter) error {
			return a.eng.Chat(ctx, sessions.GetOrCreate("feishu_"+chatID, a.workDir), prompt, r)
		})
	// 第 16 讲：高危命令挂起 → 飞书审批 → 唤醒
	bot.Approvals = permission.NewApprovalManager(10 * time.Minute)
	bot.UseCommand(feishu.ApprovalCommand(bot.Approvals, bot.Sender()))
	bot.UseCommand(func(ctx context.Context, chatID, text string) bool {
		if text != "/reset" && text != "/new" {
			return false
		}
		sessions.Reset("feishu_" + chatID)
		_ = bot.Sender().SendText(ctx, chatID, "🧹 已清空本会话的记忆。")
		return true
	})
	dispatcher := bot.EventDispatcher(os.Getenv("FEISHU_VERIFY_TOKEN"), os.Getenv("FEISHU_ENCRYPT_KEY"))

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(a.stats.Snapshot())
	})
	mux.HandleFunc("/webhook/event", httpserverext.NewEventHandlerFunc(dispatcher))
	go func() {
		log.Printf("🚀 go-tiny-claw 服务端已启动，HTTP 监听 %s (/healthz, /webhook/event)", *addr)
		if err := http.ListenAndServe(*addr, mux); err != nil {
			log.Fatalf("服务器启动失败: %v", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if mode := os.Getenv("FEISHU_CONNECTION_MODE"); mode == "" || mode == "ws" || mode == "websocket" {
		cli := larkws.NewClient(appID, appSecret, larkws.WithEventHandler(dispatcher))
		log.Println("[Feishu] 使用长连接 (WebSocket) 模式接收事件")
		go func() {
			if err := cli.Start(ctx); err != nil {
				log.Fatalf("飞书长连接失败: %v", err)
			}
		}()
	} else {
		log.Println("[Feishu] 使用 Webhook 模式，请把 http://<公网地址>/webhook/event 配置到开发者后台")
	}
	<-ctx.Done()
	log.Println("收到退出信号，等待进行中的任务结束...")
	bot.Wait()
}

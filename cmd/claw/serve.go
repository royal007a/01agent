package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log"
	"net"
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
	"github.com/weberzhao/go-tiny-claw/internal/httpapi"
	"github.com/weberzhao/go-tiny-claw/internal/permission"
)

// serve 以常驻服务方式运行：本地 HTTP API 始终开启；配置了飞书凭据时同时接入飞书（第 09/16 讲）。
//
//	CLAW_HTTP_TOKEN                          可选，HTTP API 的 Bearer Token
//	FEISHU_APP_ID / FEISHU_APP_SECRET        设置后启用飞书机器人
//	FEISHU_CONNECTION_MODE=ws|webhook        默认 ws：长连接，本地无公网 IP 也能收事件
//	FEISHU_VERIFY_TOKEN / FEISHU_ENCRYPT_KEY webhook 模式下使用
func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:48080", "HTTP 监听地址（/healthz /metrics /v1/* /webhook/event）")
	thinking := fs.Bool("thinking", false, "开启慢思考")
	planMode := fs.Bool("plan", false, "开启 Plan Mode")
	maxTurns := fs.Int("max-turns", 60, "单次任务最大轮数（硬性兜底，0 为不限制）")
	workDir := fs.String("workdir", filepath.Join(defaultStateDir(), "workspace"), "工作区目录")
	fs.Parse(args)

	// HTTP API 能驱动一个带 bash 的 Agent：监听非回环地址时必须设置 Token
	if host, _, err := net.SplitHostPort(*addr); err != nil {
		log.Fatalf("非法监听地址 %q: %v", *addr, err)
	} else if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) && os.Getenv("CLAW_HTTP_TOKEN") == "" {
		log.Fatalf("拒绝在非回环地址 %s 上无鉴权启动：请设置 CLAW_HTTP_TOKEN", *addr)
	}
	a := buildEngine(*workDir, *thinking, *planMode)
	a.eng.MaxTurns = *maxTurns
	// 每个入口会话一个 Session，JSONL 持久化，重启后自动恢复（第 11 讲）
	sessions := ctxpkg.NewSessionManager(filepath.Join(defaultStateDir(), "sessions"))
	approvals := permission.NewApprovalManager(10 * time.Minute)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(a.stats.Snapshot())
	})
	api := &httpapi.Server{Engine: a.eng, Sessions: sessions, WorkDir: a.workDir, Approvals: approvals, Token: os.Getenv("CLAW_HTTP_TOKEN")}
	api.Register(mux)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var bot *feishu.FeishuBot
	if appID, appSecret := os.Getenv("FEISHU_APP_ID"), os.Getenv("FEISHU_APP_SECRET"); appID != "" && appSecret != "" {
		bot = feishu.NewFeishuBot(feishu.NewLarkSender(appID, appSecret),
			func(ctx context.Context, chatID, prompt string, r engine.Reporter) error {
				sess := sessions.GetOrCreate("feishu_"+chatID, a.workDir)
				p0, c0, cost0 := sess.UsageSnapshot()
				err := a.eng.Chat(ctx, sess, prompt, r)
				p1, c1, cost1 := sess.UsageSnapshot()
				r.OnMessage(ctx, feishu.CostSummary(p1-p0, c1-c0, cost1-cost0, sess.UsageAvailable())) // 第 22 讲：汇报附上本次成本
				return err
			})
		// 第 22 讲思考题：群聊只响应 @ 机器人的消息（可用 FEISHU_BOT_OPEN_ID 精确到本机器人）
		bot.BotOpenID = os.Getenv("FEISHU_BOT_OPEN_ID")
		// 第 16 讲：高危命令挂起 → 飞书审批 → 唤醒
		bot.Approvals = approvals
		bot.UseCommand(feishu.ApprovalCommand(approvals, bot.Sender()))
		bot.UseCommand(func(ctx context.Context, chatID, text string) bool {
			if text != "/reset" && text != "/new" {
				return false
			}
			if bot.TryWithChatLock(chatID, func() { sessions.Reset("feishu_" + chatID) }) {
				_ = bot.Sender().SendText(ctx, chatID, "🧹 已清空本会话的记忆。")
			} else {
				_ = bot.Sender().SendText(ctx, chatID, "⏳ 本会话有任务正在执行，请结束后再 /reset。")
			}
			return true
		})
		dispatcher := bot.EventDispatcher(os.Getenv("FEISHU_VERIFY_TOKEN"), os.Getenv("FEISHU_ENCRYPT_KEY"))
		mux.HandleFunc("/webhook/event", httpserverext.NewEventHandlerFunc(dispatcher))
		if mode := os.Getenv("FEISHU_CONNECTION_MODE"); mode == "" || mode == "ws" || mode == "websocket" {
			cli := larkws.NewClient(appID, appSecret, larkws.WithEventHandler(dispatcher))
			log.Println("[Feishu] 使用长连接 (WebSocket) 模式接收事件")
			go func() {
				if err := cli.Start(ctx); err != nil && ctx.Err() == nil {
					log.Printf("[Feishu] 长连接退出: %v", err)
				}
			}()
		} else {
			log.Println("[Feishu] 使用 Webhook 模式，请把 http://<公网地址>/webhook/event 配置到开发者后台")
		}
	} else {
		log.Println("[Feishu] 未配置 FEISHU_APP_ID/FEISHU_APP_SECRET，仅启用本地 HTTP API")
	}

	srv := &http.Server{Addr: *addr, Handler: mux}
	go func() {
		log.Printf("🚀 go-tiny-claw 服务端已启动: http://%s （工作区 %s）", *addr, a.workDir)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("服务器启动失败: %v", err)
		}
	}()
	<-ctx.Done()
	log.Println("收到退出信号，等待进行中的任务结束...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	if bot != nil {
		bot.Wait()
	}
}

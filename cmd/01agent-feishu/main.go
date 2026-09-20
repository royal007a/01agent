package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	"github.com/larksuite/oapi-sdk-go/v3/channel"
	larktypes "github.com/larksuite/oapi-sdk-go/v3/channel/types"
	"github.com/larksuite/oapi-sdk-go/v3/core"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	agentfeishu "github.com/royal007a/01agent/internal/feishu"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	appID := strings.TrimSpace(os.Getenv("FEISHU_APP_ID"))
	appSecret := strings.TrimSpace(os.Getenv("FEISHU_APP_SECRET"))
	if appID == "" || appSecret == "" {
		return errors.New("FEISHU_APP_ID and FEISHU_APP_SECRET are required")
	}
	queueDir := envOr("FEISHU_QUEUE_DIR", filepath.Join(envOr("AGENT_RUN_DIR", "/tmp/01agent-runs"), "feishu"))
	store, err := agentfeishu.NewStore(queueDir)
	if err != nil {
		return fmt.Errorf("initialize feishu queue: %w", err)
	}
	httpClient := &http.Client{Timeout: envDuration("FEISHU_AGENT_HTTP_TIMEOUT", 11*time.Minute)}
	agentClient, err := agentfeishu.NewHTTPAgentClient(
		envOr("AGENT_API_URL", "http://127.0.0.1:8080"), os.Getenv("AGENT_API_TOKEN"), httpClient,
		splitCSV(os.Getenv("FEISHU_APPROVED_TOOLS")),
	)
	if err != nil {
		return err
	}

	client := lark.NewClient(appID, appSecret, lark.WithLogLevel(larkcore.LogLevelInfo))
	wsClient := larkws.NewClient(appID, appSecret, larkws.WithLogLevel(larkcore.LogLevelInfo))
	feishuChannel := channel.NewChannel(client, wsClient)
	sender := channelSender{channel: feishuChannel}
	bridge, err := agentfeishu.NewBridge(store, agentClient, sender, agentfeishu.Config{
		QueueSize: envInt("FEISHU_QUEUE_SIZE", 128), Workers: envInt("FEISHU_WORKERS", 2),
		MaxAttempts: envInt("FEISHU_MAX_ATTEMPTS", 3), JobTimeout: envDuration("FEISHU_JOB_TIMEOUT", 12*time.Minute),
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := bridge.Start(ctx); err != nil {
		return fmt.Errorf("start feishu bridge: %w", err)
	}
	defer bridge.Stop()
	feishuChannel.OnMessage(func(callbackContext context.Context, message *larktypes.NormalizedMessage) error {
		if message == nil {
			return nil
		}
		return bridge.HandleMessage(callbackContext, agentfeishu.Message{
			EventID: message.EventID, MessageID: message.MessageID, ChatID: message.ChatID,
			UserID: message.UserID, Content: message.Content,
		})
	})
	feishuChannel.OnReady(func() { log.Printf("01agent Feishu channel ready") })
	feishuChannel.OnError(func(err error) { log.Printf("Feishu channel error: %v", err) })

	startResult := make(chan error, 1)
	go func() { startResult <- feishuChannel.Start(context.Background()) }()
	select {
	case err := <-startResult:
		return err
	case <-ctx.Done():
		stopContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := feishuChannel.Stop(stopContext); err != nil {
			return fmt.Errorf("stop Feishu channel: %w", err)
		}
		if err := <-startResult; err != nil {
			return err
		}
		return nil
	}
}

type channelSender struct{ channel larktypes.Channel }

func (s channelSender) Reply(ctx context.Context, message agentfeishu.Message, content string) (string, error) {
	result, err := s.channel.Send(ctx, &larktypes.SendInput{
		ChatID: message.ChatID, ReplyMessageID: message.MessageID, Markdown: content,
	})
	if err != nil {
		return "", err
	}
	return result.MessageID, nil
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) int {
	value, err := strconv.Atoi(os.Getenv(name))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func envDuration(name string, fallback time.Duration) time.Duration {
	value, err := time.ParseDuration(os.Getenv(name))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func splitCSV(value string) []string {
	var result []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}

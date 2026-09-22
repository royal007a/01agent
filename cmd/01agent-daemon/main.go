package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/royal007a/01agent/internal/daemon"
)

func main() {
	server := flag.String("server", envOr("AGENT_SERVER_URL", "http://127.0.0.1:8080"), "01agent server base URL")
	computerID := flag.String("computer-id", os.Getenv("AGENT_COMPUTER_ID"), "registered Computer ID")
	root := flag.String("root", envOr("AGENT_DAEMON_ROOT", "/var/lib/01agent-daemon"), "daemon-managed root")
	interval := flag.Duration("poll-interval", time.Second, "outbound command poll interval")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := daemon.Run(ctx, daemon.Config{
		ServerURL: *server, Token: os.Getenv("AGENT_API_TOKEN"), ComputerID: *computerID, RootDir: *root,
		PollInterval: *interval, Tools: map[string]string{"01agent-daemon": "v1"}, Sandboxes: splitCSV(os.Getenv("AGENT_DAEMON_SANDBOXES")),
	})
	if err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
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

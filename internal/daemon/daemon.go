package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/royal007a/01agent/internal/computer"
)

var safeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type Config struct {
	ServerURL     string
	Token         string
	ComputerID    string
	RootDir       string
	PollInterval  time.Duration
	Tools         map[string]string
	Sandboxes     []string
	Executor      Executor
	MaxConcurrent int
}

type executionOutcome struct {
	command computer.Command
	result  *computer.RunResult
	err     error
}

func Run(ctx context.Context, config Config) error {
	if strings.TrimSpace(config.Token) == "" || !safeID.MatchString(config.ComputerID) {
		return errors.New("daemon token and valid computer id are required")
	}
	rootValue := strings.TrimSpace(config.RootDir)
	if rootValue == "" {
		return errors.New("daemon root is required")
	}
	root, err := filepath.Abs(rootValue)
	if err != nil || root == string(filepath.Separator) || root == "." {
		return errors.New("daemon root must be a dedicated absolute directory")
	}
	if config.PollInterval <= 0 {
		config.PollInterval = time.Second
	}
	if config.Executor == nil {
		config.Executor = ProcessExecutor{Binary: os.Getenv("AGENT_RUNTIME_BINARY"), AllowedTools: allowedToolMap(os.Getenv("AGENT_DAEMON_APPROVED_TOOLS"))}
	}
	if config.MaxConcurrent <= 0 {
		config.MaxConcurrent = 2
	}
	if err := os.MkdirAll(filepath.Join(root, "agents"), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(root, "trash"), 0o700); err != nil {
		return err
	}
	endpoint, err := daemonURL(config.ServerURL)
	if err != nil {
		return err
	}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+config.Token)
	connection, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		return err
	}
	defer connection.Close(websocket.StatusNormalClosure, "daemon stopped")
	hello := computer.Hello{ComputerID: config.ComputerID, LeaseID: newLeaseID(), OS: runtime.GOOS, Arch: runtime.GOARCH, Runtime: runtime.Version(), Tools: config.Tools, Sandboxes: config.Sandboxes}
	if err := wsjson.Write(ctx, connection, computer.ClientMessage{Type: "hello", Hello: &hello}); err != nil {
		return err
	}
	var response computer.ServerMessage
	if err := wsjson.Read(ctx, connection, &response); err != nil {
		return fmt.Errorf("read daemon hello: %w", err)
	}
	if response.Type != "hello_ack" {
		return errors.New("daemon hello was not acknowledged")
	}
	ticker := time.NewTicker(config.PollInterval)
	defer ticker.Stop()
	outcomes := make(chan executionOutcome, config.MaxConcurrent+8)
	semaphore := make(chan struct{}, config.MaxConcurrent)
	active := map[string]context.CancelFunc{}
	defer func() {
		for _, cancel := range active {
			cancel()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := poll(ctx, connection, hello, root, config, semaphore, active, outcomes); err != nil {
				return err
			}
		case outcome := <-outcomes:
			if outcome.command.Run != nil {
				delete(active, outcome.command.Run.RunID)
			}
			ack := computer.CommandAck{CommandID: outcome.command.ID, Success: outcome.err == nil, Result: outcome.result}
			if outcome.err != nil {
				ack.Error = outcome.err.Error()
			}
			if err := acknowledge(ctx, connection, ack); err != nil {
				return err
			}
		}
	}
}

func poll(ctx context.Context, connection *websocket.Conn, hello computer.Hello, root string, config Config, semaphore chan struct{}, active map[string]context.CancelFunc, outcomes chan<- executionOutcome) error {
	if err := wsjson.Write(ctx, connection, computer.ClientMessage{Type: "poll"}); err != nil {
		return err
	}
	var response computer.ServerMessage
	if err := wsjson.Read(ctx, connection, &response); err != nil {
		return err
	}
	if response.Type == "noop" {
		return nil
	}
	if response.Type != "command" || response.Command == nil {
		return errors.New("daemon received invalid server message")
	}
	command := *response.Command
	if command.Kind == "cancel_run" {
		if cancel := active[command.CancelRunID]; cancel != nil {
			cancel()
		}
		return acknowledge(ctx, connection, computer.CommandAck{CommandID: command.ID, Success: true})
	}
	var executionContext context.Context
	var cancel context.CancelFunc
	if command.Run != nil {
		executionContext, cancel = context.WithTimeout(ctx, time.Duration(command.Run.TimeoutSeconds)*time.Second)
		active[command.Run.RunID] = cancel
	} else {
		executionContext, cancel = context.WithCancel(ctx)
	}
	go func() {
		defer cancel()
		select {
		case semaphore <- struct{}{}:
			defer func() { <-semaphore }()
		case <-executionContext.Done():
			outcomes <- executionOutcome{command: command, err: executionContext.Err()}
			return
		}
		if command.Kind == "run_agent" && command.Run != nil {
			result, err := config.Executor.Execute(executionContext, root, *command.Run)
			outcomes <- executionOutcome{command: command, result: &result, err: err}
			return
		}
		outcomes <- executionOutcome{command: command, err: execute(root, command)}
	}()
	return nil
}

func acknowledge(ctx context.Context, connection *websocket.Conn, ack computer.CommandAck) error {
	if err := wsjson.Write(ctx, connection, computer.ClientMessage{Type: "ack", Ack: &ack}); err != nil {
		return err
	}
	var response computer.ServerMessage
	if err := wsjson.Read(ctx, connection, &response); err != nil {
		return err
	}
	if response.Type != "acknowledged" {
		return errors.New("command acknowledgement was not confirmed")
	}
	return nil
}

func execute(root string, command computer.Command) error {
	if command.Kind != "cleanup_agent" || !safeID.MatchString(command.AgentID) || !safeID.MatchString(command.ID) {
		return errors.New("unsupported or invalid daemon command")
	}
	agentsRoot := filepath.Join(root, "agents")
	target := filepath.Join(agentsRoot, command.AgentID)
	relative, err := filepath.Rel(agentsRoot, target)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("cleanup target escapes managed agent root")
	}
	if _, err := os.Lstat(target); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	trash := filepath.Join(root, "trash", command.ID)
	if err := os.Rename(target, trash); err != nil {
		return err
	}
	return os.RemoveAll(trash)
}

func daemonURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	switch parsed.Scheme {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", errors.New("daemon server URL must use http(s) or ws(s)")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/v2/daemon/connect"
	return parsed.String(), nil
}

func newLeaseID() string {
	var value [12]byte
	if _, err := rand.Read(value[:]); err == nil {
		return "daemon-" + hex.EncodeToString(value[:])
	}
	return fmt.Sprintf("daemon-%d", time.Now().UnixNano())
}

func allowedToolMap(value string) map[string]bool {
	result := map[string]bool{}
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result[item] = true
		}
	}
	return result
}

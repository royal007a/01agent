package feishu

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeAgent struct {
	mu    sync.Mutex
	calls int
}

func (a *fakeAgent) Turn(_ context.Context, sessionID, operationID, prompt string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	return sessionID + ":" + operationID + ":" + prompt, nil
}

type fakeSender struct {
	mu    sync.Mutex
	calls int
}

func (s *fakeSender) Reply(_ context.Context, _ Message, content string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return "reply-" + content[:8], nil
}

func TestBridgeDurablyDeduplicatesAndProcessesMessage(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	agent := &fakeAgent{}
	sender := &fakeSender{}
	bridge, err := NewBridge(store, agent, sender, Config{Workers: 1, QueueSize: 4, JobTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := bridge.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer bridge.Stop()
	message := Message{EventID: "event-1", MessageID: "message-1", ChatID: "chat-1", Content: "hello"}
	if err := bridge.HandleMessage(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	if err := bridge.HandleMessage(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		job, err := store.Get(context.Background(), jobID(message.EventID))
		if err == nil && job.State == JobSucceeded {
			if job.Attempts != 1 || job.AgentReply == "" || job.ReplyMessage == "" {
				t.Fatalf("job=%#v", job)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job did not finish: %#v err=%v", job, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	agent.mu.Lock()
	agentCalls := agent.calls
	agent.mu.Unlock()
	sender.mu.Lock()
	senderCalls := sender.calls
	sender.mu.Unlock()
	if agentCalls != 1 || senderCalls != 1 {
		t.Fatalf("agent calls=%d sender calls=%d", agentCalls, senderCalls)
	}
}

func TestBridgeReconcilesInterruptedProcessingAfterRestart(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	message := Message{EventID: "event-restart", MessageID: "message-restart", ChatID: "chat", Content: "resume"}
	job, _, err := store.Enqueue(context.Background(), message)
	if err != nil {
		t.Fatal(err)
	}
	if claimed, ok, err := store.Claim(context.Background(), job.ID); err != nil || !ok || claimed.State != JobProcessing {
		t.Fatalf("claimed=%#v ok=%v err=%v", claimed, ok, err)
	}
	agent := &fakeAgent{}
	bridge, err := NewBridge(store, agent, &fakeSender{}, Config{Workers: 1, JobTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := bridge.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer bridge.Stop()
	deadline := time.Now().Add(2 * time.Second)
	for {
		current, err := store.Get(context.Background(), job.ID)
		if err == nil && current.State == JobSucceeded {
			if current.Attempts != 2 {
				t.Fatalf("attempts=%d", current.Attempts)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job did not recover: %#v err=%v", current, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHTTPAgentClientUsesSessionOperationIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/base/v1/sessions/session-1/turns" || request.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("request=%s auth=%q", request.URL.Path, request.Header.Get("Authorization"))
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["operation_id"] != "event-1" || body["prompt"] != "hello" {
			t.Errorf("body=%#v", body)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"result":{"reason":"completed","final_message":{"content":"answer"}}}`))
	}))
	defer server.Close()
	client, err := NewHTTPAgentClient(server.URL+"/base", "secret", server.Client(), []string{"edit_file"})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := client.Turn(context.Background(), "session-1", "event-1", "hello")
	if err != nil || reply != "answer" {
		t.Fatalf("reply=%q err=%v", reply, err)
	}
}

func TestStoreRejectsConflictingDuplicateEvent(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	message := Message{EventID: "same", MessageID: "one", ChatID: "chat", Content: "first"}
	if _, created, err := store.Enqueue(context.Background(), message); err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	message.Content = "different"
	if _, _, err := store.Enqueue(context.Background(), message); err == nil || !strings.Contains(err.Error(), "different semantics") {
		t.Fatalf("conflict error=%v", err)
	}
}

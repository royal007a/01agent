package dispatcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/royal007a/01agent/internal/agentregistry"
	"github.com/royal007a/01agent/internal/computer"
	"github.com/royal007a/01agent/internal/workitem"
)

type protocolReply struct {
	ackSuccess bool
	runSuccess bool
	decision   string
	errorText  string
}

type dispatchHarness struct {
	root       string
	tasks      *workitem.Store
	agents     *agentregistry.Store
	computers  *computer.Store
	dispatcher *Store
	leases     map[string]string
	calls      map[string]int
}

func TestDispatcherEndToEndFaultMatrix(t *testing.T) {
	t.Run("01_parent_child_two_computers_independent_review", func(t *testing.T) {
		h := newDispatchHarness(t)
		h.createTask(t, "parent", "", "agent-a", "agent-b")
		h.createTask(t, "child", "parent", "agent-b", "agent-a")
		h.start(t, "parent-run", "parent", 3)
		h.start(t, "child-run", "child", 3)
		h.drive(t, func() bool { return h.phase(t, "parent-run") == Succeeded && h.phase(t, "child-run") == Succeeded }, nil)
		parent, _ := h.tasks.Get(context.Background(), "parent")
		child, _ := h.tasks.Get(context.Background(), "child")
		if parent.State != workitem.Done || child.State != workitem.Done || h.calls["computer-a:review"] == 0 || h.calls["computer-b:review"] == 0 {
			t.Fatalf("parent=%s child=%s calls=%v", parent.State, child.State, h.calls)
		}
	})

	t.Run("02_start_operation_is_idempotent", func(t *testing.T) {
		h := newDispatchHarness(t)
		h.createTask(t, "task", "", "agent-a", "agent-b")
		first := h.start(t, "execution", "task", 3)
		second, err := h.dispatcher.Start(context.Background(), StartInput{OperationID: "start-execution", ID: "execution", TaskID: "task", MaxAttempts: 3, TimeoutSeconds: 30})
		if err != nil || first.ID != second.ID || first.Revision != second.Revision {
			t.Fatalf("first=%+v second=%+v err=%v", first, second, err)
		}
	})

	t.Run("03_same_agent_cannot_self_review", func(t *testing.T) {
		h := newDispatchHarness(t)
		h.createTask(t, "task", "", "agent-a", "agent-a")
		_, err := h.dispatcher.Start(context.Background(), StartInput{OperationID: "start-self", ID: "execution", TaskID: "task", MaxAttempts: 3, TimeoutSeconds: 30})
		if err == nil || !strings.Contains(err.Error(), "distinct") {
			t.Fatalf("self review was accepted: %v", err)
		}
	})

	t.Run("04_offline_computer_waits_then_recovers", func(t *testing.T) {
		h := newDispatchHarness(t)
		h.createTask(t, "task", "", "agent-a", "agent-b")
		h.start(t, "execution", "task", 3)
		if err := h.computers.Disconnect(context.Background(), "computer-a", h.leases["computer-a"]); err != nil {
			t.Fatal(err)
		}
		delete(h.leases, "computer-a")
		_, _ = h.dispatcher.Reconcile(context.Background())
		if phase := h.phase(t, "execution"); phase != PendingClaim {
			t.Fatalf("offline phase=%s", phase)
		}
		h.connect(t, "computer-a", "lease-a2")
		h.drive(t, func() bool { return h.phase(t, "execution") == Succeeded }, nil)
	})

	t.Run("05_runtime_failure_retries_and_succeeds", func(t *testing.T) {
		h := newDispatchHarness(t)
		h.createTask(t, "task", "", "agent-a", "agent-b")
		h.start(t, "execution", "task", 3)
		h.drive(t, func() bool { return h.phase(t, "execution") == Succeeded }, func(request computer.RunRequest, call int) protocolReply {
			if request.Mode == computer.RunExecute && call == 1 {
				return protocolReply{ackSuccess: true, runSuccess: false, errorText: "provider timeout"}
			}
			return defaultReply(request)
		})
		if h.execution(t, "execution").Attempt != 2 {
			t.Fatalf("attempts=%d", h.execution(t, "execution").Attempt)
		}
	})

	t.Run("06_daemon_ack_failure_retries", func(t *testing.T) {
		h := newDispatchHarness(t)
		h.createTask(t, "task", "", "agent-a", "agent-b")
		h.start(t, "execution", "task", 3)
		h.drive(t, func() bool { return h.phase(t, "execution") == Succeeded }, func(request computer.RunRequest, call int) protocolReply {
			if request.Mode == computer.RunExecute && call == 1 {
				return protocolReply{ackSuccess: false, errorText: "daemon disconnected"}
			}
			return defaultReply(request)
		})
	})

	t.Run("07_execution_attempts_exhaust_and_close_task", func(t *testing.T) {
		h := newDispatchHarness(t)
		h.createTask(t, "task", "", "agent-a", "agent-b")
		h.start(t, "execution", "task", 2)
		h.drive(t, func() bool { return h.phase(t, "execution") == Failed }, func(request computer.RunRequest, _ int) protocolReply {
			if request.Mode == computer.RunExecute {
				return protocolReply{ackSuccess: true, runSuccess: false, errorText: "permanent failure"}
			}
			return defaultReply(request)
		})
		task, _ := h.tasks.Get(context.Background(), "task")
		if task.State != workitem.Closed {
			t.Fatalf("task state=%s", task.State)
		}
	})

	t.Run("08_review_runtime_failure_retries", func(t *testing.T) {
		h := newDispatchHarness(t)
		h.createTask(t, "task", "", "agent-a", "agent-b")
		h.start(t, "execution", "task", 3)
		h.drive(t, func() bool { return h.phase(t, "execution") == Succeeded }, func(request computer.RunRequest, call int) protocolReply {
			if request.Mode == computer.RunReview && call == 1 {
				return protocolReply{ackSuccess: true, runSuccess: false, errorText: "review timeout"}
			}
			return defaultReply(request)
		})
		if h.execution(t, "execution").ReviewAttempt != 2 {
			t.Fatalf("review attempts=%d", h.execution(t, "execution").ReviewAttempt)
		}
	})

	t.Run("09_reject_triggers_rework_then_pass", func(t *testing.T) {
		h := newDispatchHarness(t)
		h.createTask(t, "task", "", "agent-a", "agent-b")
		h.start(t, "execution", "task", 3)
		h.drive(t, func() bool { return h.phase(t, "execution") == Succeeded }, func(request computer.RunRequest, call int) protocolReply {
			if request.Mode == computer.RunReview && call == 1 {
				return protocolReply{ackSuccess: true, runSuccess: true, decision: "reject", errorText: "missing regression evidence"}
			}
			return defaultReply(request)
		})
		if h.execution(t, "execution").ReworkCount != 1 {
			t.Fatalf("rework count=%d", h.execution(t, "execution").ReworkCount)
		}
	})

	t.Run("10_needs_human_stops_automatic_progress", func(t *testing.T) {
		h := newDispatchHarness(t)
		h.createTask(t, "task", "", "agent-a", "agent-b")
		h.start(t, "execution", "task", 3)
		h.drive(t, func() bool { return h.phase(t, "execution") == AwaitingHuman }, func(request computer.RunRequest, _ int) protocolReply {
			if request.Mode == computer.RunReview {
				return protocolReply{ackSuccess: true, runSuccess: true, decision: "needs_human", errorText: "policy decision"}
			}
			return defaultReply(request)
		})
		before := h.execution(t, "execution").Revision
		for range 3 {
			_, _ = h.dispatcher.Reconcile(context.Background())
		}
		if h.execution(t, "execution").Revision != before {
			t.Fatal("awaiting-human execution changed without a human decision")
		}
	})

	t.Run("11_manual_gate_pass_completes_awaiting_human", func(t *testing.T) {
		h := newDispatchHarness(t)
		h.createTask(t, "task", "", "agent-a", "agent-b")
		h.start(t, "execution", "task", 3)
		h.drive(t, func() bool { return h.phase(t, "execution") == AwaitingHuman }, func(request computer.RunRequest, _ int) protocolReply {
			if request.Mode == computer.RunReview {
				return protocolReply{ackSuccess: true, runSuccess: true, decision: "needs_human", errorText: "policy decision"}
			}
			return defaultReply(request)
		})
		task, _ := h.tasks.Get(context.Background(), "task")
		latest := task.Submissions[len(task.Submissions)-1]
		if _, err := h.tasks.Review(context.Background(), task.ID, "manual-review", task.Revision, workitem.GateResult{Decision: workitem.GatePass, ReviewerID: "agent-b", ArtifactVersion: latest.Artifacts, Evidence: []string{"human approved"}, Reason: "accepted"}); err != nil {
			t.Fatal(err)
		}
		h.drive(t, func() bool { return h.phase(t, "execution") == Succeeded }, nil)
	})

	t.Run("12_cancel_running_run_closes_task", func(t *testing.T) {
		h := newDispatchHarness(t)
		h.createTask(t, "task", "", "agent-a", "agent-b")
		h.start(t, "execution", "task", 3)
		_, _ = h.dispatcher.Reconcile(context.Background())
		if h.phase(t, "execution") != Running {
			t.Fatal("run was not dispatched")
		}
		if _, err := h.dispatcher.Cancel(context.Background(), "execution", CancelInput{OperationID: "cancel-execution", Reason: "user canceled"}); err != nil {
			t.Fatal(err)
		}
		h.drive(t, func() bool { return h.phase(t, "execution") == Canceled }, nil)
		task, _ := h.tasks.Get(context.Background(), "task")
		if task.State != workitem.Closed {
			t.Fatalf("task state=%s", task.State)
		}
	})

	t.Run("13_late_result_after_cancel_is_ignored", func(t *testing.T) {
		h := newDispatchHarness(t)
		h.createTask(t, "task", "", "agent-a", "agent-b")
		h.start(t, "execution", "task", 3)
		_, _ = h.dispatcher.Reconcile(context.Background())
		command, err := h.computers.PollCommand(context.Background(), "computer-a", h.leases["computer-a"])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.dispatcher.Cancel(context.Background(), "execution", CancelInput{OperationID: "cancel-late", Reason: "superseded"}); err != nil {
			t.Fatal(err)
		}
		result := successfulResult(*command.Run, "")
		if _, err := h.computers.AckCommand(context.Background(), "computer-a", h.leases["computer-a"], computer.CommandAck{CommandID: command.ID, Success: true, Result: &result}); err != nil {
			t.Fatal(err)
		}
		h.drive(t, func() bool { return h.phase(t, "execution") == Canceled }, nil)
		task, _ := h.tasks.Get(context.Background(), "task")
		if len(task.ArtifactRefs) != 0 || task.State != workitem.Closed {
			t.Fatalf("late result committed: state=%s artifacts=%v", task.State, task.ArtifactRefs)
		}
	})

	t.Run("14_sent_command_redelivers_only_to_new_daemon_lease", func(t *testing.T) {
		h := newDispatchHarness(t)
		h.createTask(t, "task", "", "agent-a", "agent-b")
		h.start(t, "execution", "task", 3)
		_, _ = h.dispatcher.Reconcile(context.Background())
		first, err := h.computers.PollCommand(context.Background(), "computer-a", h.leases["computer-a"])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.computers.PollCommand(context.Background(), "computer-a", h.leases["computer-a"]); !errors.Is(err, computer.ErrNoCommand) {
			t.Fatalf("same lease received duplicate: %v", err)
		}
		if err := h.computers.Disconnect(context.Background(), "computer-a", h.leases["computer-a"]); err != nil {
			t.Fatal(err)
		}
		h.connect(t, "computer-a", "lease-a2")
		second, err := h.computers.PollCommand(context.Background(), "computer-a", "lease-a2")
		if err != nil || second.ID != first.ID || second.Attempt != 2 {
			t.Fatalf("redelivery=%+v err=%v", second, err)
		}
		result := successfulResult(*second.Run, "")
		if _, err := h.computers.AckCommand(context.Background(), "computer-a", "lease-a2", computer.CommandAck{CommandID: second.ID, Success: true, Result: &result}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("15_restart_replays_canonical_dispatch_state", func(t *testing.T) {
		h := newDispatchHarness(t)
		h.createTask(t, "task", "", "agent-a", "agent-b")
		h.start(t, "execution", "task", 3)
		_, _ = h.dispatcher.Reconcile(context.Background())
		restarted, err := New(filepath.Join(h.root, "dispatcher"), h.tasks, h.agents, h.computers)
		if err != nil {
			t.Fatal(err)
		}
		h.dispatcher = restarted
		h.drive(t, func() bool { return h.phase(t, "execution") == Succeeded }, nil)
	})
}

func newDispatchHarness(t *testing.T) *dispatchHarness {
	t.Helper()
	root := t.TempDir()
	tasks, err := workitem.New(filepath.Join(root, "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	agents, err := agentregistry.New(filepath.Join(root, "agents"))
	if err != nil {
		t.Fatal(err)
	}
	computers, err := computer.New(filepath.Join(root, "computers"))
	if err != nil {
		t.Fatal(err)
	}
	dispatches, err := New(filepath.Join(root, "dispatcher"), tasks, agents, computers)
	if err != nil {
		t.Fatal(err)
	}
	h := &dispatchHarness{root: root, tasks: tasks, agents: agents, computers: computers, dispatcher: dispatches, leases: map[string]string{}, calls: map[string]int{}}
	for _, id := range []string{"agent-a", "agent-b"} {
		if _, err := agents.Create(context.Background(), agentregistry.CreateInput{OperationID: "create-" + id, ID: id, WorkspaceID: "workspace", Name: id, CreatedBy: "owner", PromptRef: "prompts/" + id + ".md", Model: "test", MyRole: "worker", SessionID: "session-" + id}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"computer-a", "computer-b"} {
		if _, err := computers.Register(context.Background(), computer.RegisterInput{OperationID: "register-" + id, ID: id, OwnerID: "owner", Name: id}); err != nil {
			t.Fatal(err)
		}
		h.connect(t, id, "lease-"+strings.TrimPrefix(id, "computer-"))
	}
	if _, _, err := computers.Rebind(context.Background(), computer.RebindInput{OperationID: "bind-agent-a", AgentID: "agent-a", TargetComputerID: "computer-a", ActorID: "owner", ExpectedBindingRevision: 0}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := computers.Rebind(context.Background(), computer.RebindInput{OperationID: "bind-agent-b", AgentID: "agent-b", TargetComputerID: "computer-b", ActorID: "owner", ExpectedBindingRevision: 0}); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *dispatchHarness) connect(t *testing.T, computerID, leaseID string) {
	t.Helper()
	if _, err := h.computers.Connect(context.Background(), computer.Hello{ComputerID: computerID, LeaseID: leaseID, OS: "linux", Arch: "amd64", Runtime: "test", Tools: map[string]string{"runtime": "test"}, Sandboxes: []string{"test"}}, time.Minute); err != nil {
		t.Fatal(err)
	}
	h.leases[computerID] = leaseID
}

func (h *dispatchHarness) createTask(t *testing.T, id, parent, assignee, reviewer string) workitem.Task {
	t.Helper()
	task, err := h.tasks.Create(context.Background(), workitem.Create{OperationID: "create-" + id, ID: id, WorkspaceID: "workspace", ChannelID: "channel", ParentTaskID: parent, CreatorID: "owner", Title: id, Objective: "complete " + id, Requirements: []workitem.Requirement{{ID: "R1", Text: "produce evidence"}}, Scope: workitem.Scope{Allow: []string{"."}}, StopConditions: []string{"evidence produced"}, AssigneeID: assignee, Gate: workitem.GateSpec{Kind: workitem.GateAgent, ReviewerID: reviewer, Checks: []string{"verify R1"}, RequiredEvidence: []string{"artifact"}, OnReject: "return to author"}})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func (h *dispatchHarness) start(t *testing.T, id, taskID string, attempts int) Execution {
	t.Helper()
	execution, err := h.dispatcher.Start(context.Background(), StartInput{OperationID: "start-" + id, ID: id, TaskID: taskID, MaxAttempts: attempts, TimeoutSeconds: 30})
	if err != nil {
		t.Fatal(err)
	}
	return execution
}

func (h *dispatchHarness) drive(t *testing.T, done func() bool, reply func(computer.RunRequest, int) protocolReply) {
	t.Helper()
	if reply == nil {
		reply = func(request computer.RunRequest, _ int) protocolReply { return defaultReply(request) }
	}
	for range 150 {
		if _, err := h.dispatcher.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
		h.serve(t, reply)
		if done() {
			return
		}
	}
	t.Fatalf("dispatcher did not reach expected condition; executions=%+v", h.executions(t))
}

func (h *dispatchHarness) serve(t *testing.T, reply func(computer.RunRequest, int) protocolReply) {
	t.Helper()
	for computerID, leaseID := range h.leases {
		for {
			command, err := h.computers.PollCommand(context.Background(), computerID, leaseID)
			if errors.Is(err, computer.ErrNoCommand) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if command.Kind == "cancel_run" || command.Kind == "cleanup_agent" {
				if _, err := h.computers.AckCommand(context.Background(), computerID, leaseID, computer.CommandAck{CommandID: command.ID, Success: true}); err != nil {
					t.Fatal(err)
				}
				continue
			}
			key := computerID + ":" + string(command.Run.Mode)
			h.calls[key]++
			response := reply(*command.Run, h.calls[key])
			if !response.ackSuccess {
				if _, err := h.computers.AckCommand(context.Background(), computerID, leaseID, computer.CommandAck{CommandID: command.ID, Success: false, Error: response.errorText}); err != nil {
					t.Fatal(err)
				}
				continue
			}
			result := successfulResult(*command.Run, response.decision)
			result.Success = response.runSuccess
			if response.errorText != "" {
				result.Terminal, result.Reason = response.errorText, response.errorText
			}
			if _, err := h.computers.AckCommand(context.Background(), computerID, leaseID, computer.CommandAck{CommandID: command.ID, Success: true, Result: &result}); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func defaultReply(request computer.RunRequest) protocolReply {
	decision := ""
	if request.Mode == computer.RunReview {
		decision = "pass"
	}
	return protocolReply{ackSuccess: true, runSuccess: true, decision: decision}
}

func successfulResult(request computer.RunRequest, decision string) computer.RunResult {
	now := time.Now().UTC()
	digest := sha256.Sum256([]byte(request.RunID))
	reason := "completed"
	if decision != "" {
		reason = "independent review completed"
	}
	return computer.RunResult{RunID: request.RunID, TaskID: request.TaskID, Success: true, Terminal: "completed", Summary: "completed " + request.TaskID, Evidence: []string{"verified " + request.RunID}, ArtifactURI: "memory://" + request.RunID, ArtifactHash: hex.EncodeToString(digest[:]), Decision: decision, Reason: reason, StartedAt: now, CompletedAt: now}
}

func (h *dispatchHarness) execution(t *testing.T, id string) Execution {
	t.Helper()
	for _, execution := range h.executions(t) {
		if execution.ID == id {
			return execution
		}
	}
	t.Fatalf("execution %s not found", id)
	return Execution{}
}

func (h *dispatchHarness) executions(t *testing.T) []Execution {
	t.Helper()
	executions, err := h.dispatcher.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return executions
}

func (h *dispatchHarness) phase(t *testing.T, id string) Phase {
	t.Helper()
	return h.execution(t, id).Phase
}

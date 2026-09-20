package feishu

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

type AgentClient interface {
	Turn(context.Context, string, string, string) (string, error)
}

type ReplySender interface {
	Reply(context.Context, Message, string) (string, error)
}

type Bridge struct {
	store       *Store
	agent       AgentClient
	sender      ReplySender
	queue       chan string
	workers     int
	maxAttempts int
	jobTimeout  time.Duration
	cancel      context.CancelFunc
	wait        sync.WaitGroup
}

type Config struct {
	QueueSize   int
	Workers     int
	MaxAttempts int
	JobTimeout  time.Duration
}

func NewBridge(store *Store, agent AgentClient, sender ReplySender, config Config) (*Bridge, error) {
	if store == nil || agent == nil || sender == nil {
		return nil, errors.New("feishu bridge requires store, agent client, and sender")
	}
	if config.QueueSize <= 0 {
		config.QueueSize = 128
	}
	if config.Workers <= 0 {
		config.Workers = 2
	}
	if config.MaxAttempts <= 0 {
		config.MaxAttempts = 3
	}
	if config.JobTimeout <= 0 {
		config.JobTimeout = 12 * time.Minute
	}
	return &Bridge{
		store: store, agent: agent, sender: sender, queue: make(chan string, config.QueueSize),
		workers: config.Workers, maxAttempts: config.MaxAttempts, jobTimeout: config.JobTimeout,
	}, nil
}

func (b *Bridge) Start(parent context.Context) error {
	if b.cancel != nil {
		return errors.New("feishu bridge is already started")
	}
	ctx, cancel := context.WithCancel(parent)
	b.cancel = cancel
	pending, err := b.store.Reconcile(context.WithoutCancel(ctx))
	if err != nil {
		cancel()
		b.cancel = nil
		return err
	}
	for index := 0; index < b.workers; index++ {
		b.wait.Add(1)
		go b.worker(ctx)
	}
	b.wait.Add(1)
	go b.dispatcher(ctx)
	for _, job := range pending {
		b.schedule(job.ID)
	}
	return nil
}

func (b *Bridge) dispatcher(ctx context.Context) {
	defer b.wait.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			jobs, err := b.store.Pending(ctx)
			if err != nil {
				continue
			}
			for _, job := range jobs {
				b.schedule(job.ID)
			}
		}
	}
}

// HandleMessage performs only a durable enqueue and a non-blocking wake-up so
// the Feishu event callback can acknowledge delivery quickly.
func (b *Bridge) HandleMessage(ctx context.Context, message Message) error {
	job, _, err := b.store.Enqueue(ctx, message)
	if err != nil {
		return err
	}
	if job.State == JobQueued {
		b.schedule(job.ID)
	}
	return nil
}

func (b *Bridge) Stop() {
	if b.cancel != nil {
		b.cancel()
		b.wait.Wait()
		b.cancel = nil
	}
}

func (b *Bridge) schedule(id string) {
	select {
	case b.queue <- id:
	default:
		// The durable queued state is authoritative. A later duplicate event or
		// restart reconciliation will wake it without losing the message.
	}
}

func (b *Bridge) worker(ctx context.Context) {
	defer b.wait.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-b.queue:
			b.process(ctx, id)
		}
	}
}

func (b *Bridge) process(parent context.Context, id string) {
	job, claimed, err := b.store.Claim(context.WithoutCancel(parent), id)
	if err != nil || !claimed {
		return
	}
	ctx, cancel := context.WithTimeout(parent, b.jobTimeout)
	reply, err := b.agent.Turn(ctx, job.SessionID, job.OperationID, job.Message.Content)
	if err == nil {
		var replyMessageID string
		replyMessageID, err = b.sender.Reply(ctx, job.Message, reply)
		if err == nil {
			_, _ = b.store.Complete(context.WithoutCancel(parent), id, reply, replyMessageID)
			cancel()
			return
		}
	}
	cancel()
	retry := job.Attempts < b.maxAttempts && parent.Err() == nil
	_, _ = b.store.Fail(context.WithoutCancel(parent), id, fmt.Sprint(err), retry)
	if retry {
		timer := time.NewTimer(time.Duration(job.Attempts) * 250 * time.Millisecond)
		select {
		case <-timer.C:
			b.schedule(id)
		case <-parent.Done():
			if !timer.Stop() {
				<-timer.C
			}
		}
	}
}

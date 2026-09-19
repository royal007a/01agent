package provider

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/royal007a/01agent/internal/schema"
)

type flakyProvider struct {
	calls atomic.Int32
	fail  int32
}

func (p *flakyProvider) Generate(context.Context, []schema.Message, []schema.ToolDefinition) (schema.Generation, error) {
	if p.calls.Add(1) <= p.fail {
		return schema.Generation{}, temporaryError{}
	}
	return schema.Generation{Message: schema.Message{Content: "ok"}}, nil
}

type temporaryError struct{}

func (temporaryError) Error() string   { return "temporary" }
func (temporaryError) Timeout() bool   { return false }
func (temporaryError) Temporary() bool { return true }

func TestResilientProviderRetriesTransientFailure(t *testing.T) {
	inner := &flakyProvider{fail: 2}
	model := WithResilience(inner, ResilienceConfig{
		MaxAttempts: 3,
		BaseDelay:   time.Millisecond,
		MaxDelay:    time.Millisecond,
		sleep:       func(context.Context, time.Duration) error { return nil },
	})
	result, err := model.Generate(context.Background(), nil, nil)
	if err != nil || result.Message.Content != "ok" || inner.calls.Load() != 3 {
		t.Fatalf("result=%#v calls=%d err=%v", result, inner.calls.Load(), err)
	}
}

func TestResilientProviderDoesNotRetryPermanentFailure(t *testing.T) {
	inner := &permanentProvider{}
	model := WithResilience(inner, ResilienceConfig{MaxAttempts: 4, sleep: func(context.Context, time.Duration) error { return nil }})
	_, err := model.Generate(context.Background(), nil, nil)
	if !errors.Is(err, errPermanent) || inner.calls.Load() != 1 {
		t.Fatalf("calls=%d err=%v", inner.calls.Load(), err)
	}
}

var errPermanent = errors.New("permanent")

type permanentProvider struct{ calls atomic.Int32 }

func (p *permanentProvider) Generate(context.Context, []schema.Message, []schema.ToolDefinition) (schema.Generation, error) {
	p.calls.Add(1)
	return schema.Generation{}, errPermanent
}

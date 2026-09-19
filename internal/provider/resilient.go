package provider

import (
	"context"
	"errors"
	"math"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	openai "github.com/openai/openai-go/v3"
	"github.com/royal007a/01agent/internal/schema"
)

type ResilienceConfig struct {
	MaxAttempts       int
	BaseDelay         time.Duration
	MaxDelay          time.Duration
	MinRequestSpacing time.Duration
	MaxConcurrent     int
	sleep             func(context.Context, time.Duration) error
}

type resilientProvider struct {
	inner     LLMProvider
	config    ResilienceConfig
	semaphore chan struct{}
	rateMu    sync.Mutex
	nextStart time.Time
}

func WithResilience(inner LLMProvider, config ResilienceConfig) LLMProvider {
	if config.MaxAttempts <= 0 {
		config.MaxAttempts = 3
	}
	if config.BaseDelay <= 0 {
		config.BaseDelay = 250 * time.Millisecond
	}
	if config.MaxDelay <= 0 {
		config.MaxDelay = 5 * time.Second
	}
	if config.MaxConcurrent <= 0 {
		config.MaxConcurrent = 4
	}
	if config.sleep == nil {
		config.sleep = sleepContext
	}
	return &resilientProvider{inner: inner, config: config, semaphore: make(chan struct{}, config.MaxConcurrent)}
}

func (p *resilientProvider) Generate(ctx context.Context, messages []schema.Message, definitions []schema.ToolDefinition) (schema.Generation, error) {
	select {
	case p.semaphore <- struct{}{}:
		defer func() { <-p.semaphore }()
	case <-ctx.Done():
		return schema.Generation{}, ctx.Err()
	}

	var lastErr error
	for attempt := 1; attempt <= p.config.MaxAttempts; attempt++ {
		if err := p.waitForRateSlot(ctx); err != nil {
			return schema.Generation{}, err
		}
		generation, err := p.inner.Generate(ctx, messages, definitions)
		if err == nil {
			return generation, nil
		}
		lastErr = err
		retryable, retryAfter := retryDecision(err)
		if !retryable || attempt == p.config.MaxAttempts {
			return schema.Generation{}, err
		}
		delay := time.Duration(float64(p.config.BaseDelay) * math.Pow(2, float64(attempt-1)))
		if delay > p.config.MaxDelay {
			delay = p.config.MaxDelay
		}
		if retryAfter > delay {
			delay = retryAfter
		}
		if err := p.config.sleep(ctx, delay); err != nil {
			return schema.Generation{}, err
		}
	}
	return schema.Generation{}, lastErr
}

func (p *resilientProvider) waitForRateSlot(ctx context.Context) error {
	if p.config.MinRequestSpacing <= 0 {
		return nil
	}
	p.rateMu.Lock()
	now := time.Now()
	wait := p.nextStart.Sub(now)
	if wait < 0 {
		wait = 0
	}
	p.nextStart = now.Add(wait).Add(p.config.MinRequestSpacing)
	p.rateMu.Unlock()
	return p.config.sleep(ctx, wait)
}

func retryDecision(err error) (bool, time.Duration) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false, 0
	}
	var networkError net.Error
	if errors.As(err, &networkError) && (networkError.Timeout() || networkError.Temporary()) {
		return true, 0
	}
	var openAIError *openai.Error
	if errors.As(err, &openAIError) {
		return retryableStatus(openAIError.StatusCode), retryAfter(openAIError.Response)
	}
	var anthropicError *anthropic.Error
	if errors.As(err, &anthropicError) {
		return retryableStatus(anthropicError.StatusCode), retryAfter(anthropicError.Response)
	}
	return false, 0
}

func retryableStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func retryAfter(response *http.Response) time.Duration {
	if response == nil {
		return 0
	}
	value := response.Header.Get("Retry-After")
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		if delay := time.Until(at); delay > 0 {
			return delay
		}
	}
	return 0
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Package runctx 通过 context.Context 在一次 Agent 运行的各层之间透传运行时信息
// （当前会话、当前 Reporter），让 Middleware / Tracker 这些"横切关注点"不必改动接口签名。
package runctx

import "context"

type key int

const (
	reporterKey key = iota
	sessionKey
)

// WithReporter 把当前运行的 Reporter 放进 ctx。
func WithReporter(ctx context.Context, r any) context.Context {
	return context.WithValue(ctx, reporterKey, r)
}

// Reporter 取出当前运行的 Reporter（可能为 nil）。
func Reporter(ctx context.Context) any { return ctx.Value(reporterKey) }

// WithSession 把当前会话放进 ctx（类型由调用方约定，避免循环依赖）。
func WithSession(ctx context.Context, s any) context.Context {
	return context.WithValue(ctx, sessionKey, s)
}

// Session 取出当前会话（可能为 nil）。
func Session(ctx context.Context) any { return ctx.Value(sessionKey) }

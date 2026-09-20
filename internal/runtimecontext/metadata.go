package runtimecontext

import "context"

type Metadata struct {
	Scope string
	RunID string
	Turn  int
}

type metadataKey struct{}

func WithMetadata(ctx context.Context, metadata Metadata) context.Context {
	return context.WithValue(ctx, metadataKey{}, metadata)
}

func FromContext(ctx context.Context) (Metadata, bool) {
	metadata, ok := ctx.Value(metadataKey{}).(Metadata)
	return metadata, ok
}

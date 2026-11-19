package entry

import (
	"context"
	"net/http"
	"sync/atomic"
)

type Meta struct {
	Protocol     string
	StreamID     string
	ConnectionID string
	RemoteAddr   string
	H2Priority   H2Priority
}

type H2Priority struct {
	Weight    uint8
	StreamDep uint32
	Exclusive bool
	Set       bool
}

type contextKey struct{}

func WithMeta(ctx context.Context, meta Meta) context.Context {
	return context.WithValue(ctx, contextKey{}, meta)
}

func FromContext(ctx context.Context) (Meta, bool) {
	m, ok := ctx.Value(contextKey{}).(Meta)
	return m, ok
}

func ProtocolOf(r *http.Request) string {
	if m, ok := FromContext(r.Context()); ok {
		return m.Protocol
	}
	if r.ProtoMajor == 3 {
		return "h3"
	}
	return "h2"
}

type ConnectionInfo struct {
	ID         string
	RemoteAddr string
	seq        atomic.Uint64
}

func (c *ConnectionInfo) NextStreamID() uint64 {
	return c.seq.Add(1)
}

type connectionKey struct{}

func WithConnectionInfo(ctx context.Context, info *ConnectionInfo) context.Context {
	return context.WithValue(ctx, connectionKey{}, info)
}

func ConnectionInfoFromContext(ctx context.Context) (*ConnectionInfo, bool) {
	info, ok := ctx.Value(connectionKey{}).(*ConnectionInfo)
	return info, ok
}

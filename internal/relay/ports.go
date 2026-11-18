package relay

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"
)

type portAllocator struct {
	mu        sync.Mutex
	bindIP    string
	min, max  int
	next      int
	cooldowns map[int]time.Time
}

func newPortAllocator(bindIP string, min, max int, cooldowns map[int]time.Time) *portAllocator {
	return &portAllocator{
		bindIP:    bindIP,
		min:       min,
		max:       max,
		next:      min,
		cooldowns: cooldowns,
	}
}

func (a *portAllocator) setCooldown(port int, until time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if old, ok := a.cooldowns[port]; !ok || until.After(old) {
		a.cooldowns[port] = until
	}
}

func (a *portAllocator) cooldownUntil(port int) time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cooldowns[port]
}

// Allocate reserves a port that is globally unique across UDP and TCP. The
// caller's used function is evaluated while the allocator lock is held.
func (a *portAllocator) Allocate(ctx context.Context, proto string, used func(int) bool) (int, net.PacketConn, net.Listener, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	capacity := a.max - a.min + 1
	now := time.Now()
	for i := 0; i < capacity; i++ {
		if err := ctx.Err(); err != nil {
			return 0, nil, nil, err
		}
		port := a.next
		a.next++
		if a.next > a.max {
			a.next = a.min
		}
		if used(port) {
			continue
		}
		if until, cooling := a.cooldowns[port]; cooling && now.Before(until) {
			continue
		}
		addr := net.JoinHostPort(a.bindIP, fmt.Sprint(port))
		switch proto {
		case "udp":
			pc, err := net.ListenPacket("udp4", addr)
			if err != nil {
				continue
			}
			return port, pc, nil, nil
		case "tcp":
			ln, err := net.Listen("tcp4", addr)
			if err != nil {
				continue
			}
			return port, nil, ln, nil
		default:
			return 0, nil, nil, fmt.Errorf("unsupported protocol %q", proto)
		}
	}
	return 0, nil, nil, fmt.Errorf("no free %s mapping port in %d-%d", proto, a.min, a.max)
}

func listenRestoredUDP(bindIP string, port int) (net.PacketConn, error) {
	return net.ListenPacket("udp4", net.JoinHostPort(bindIP, fmt.Sprint(port)))
}

func listenRestoredTCP(bindIP string, port int) (net.Listener, error) {
	return net.Listen("tcp4", net.JoinHostPort(bindIP, fmt.Sprint(port)))
}

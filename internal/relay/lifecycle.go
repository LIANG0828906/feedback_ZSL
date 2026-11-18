package relay

import (
	"context"
	"time"

	"parkrelay/internal/store"
)

func (r *Relay) sweepLoop() {
	defer r.wg.Done()
	ticker := time.NewTicker(r.cfg.SweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case now := <-ticker.C:
			var closeNow []*mapping
			r.regMu.Lock()
			for _, m := range r.mappings {
				m.mu.Lock()
				if m.recycled {
					m.mu.Unlock()
					continue
				}
				if m.active && !now.Before(m.LeaseExpires) {
					m.active = false
					m.draining = true
					m.drainDeadline = now.Add(r.cfg.InFlightDrain)
				}
				if m.draining && !now.Before(m.drainDeadline) {
					m.recycled = true
					delete(r.mappings, mappingKey(m.GatewayID, m.Protocol))
					closeNow = append(closeNow, m)
				}
				m.mu.Unlock()
			}
			r.regMu.Unlock()
			for _, m := range closeNow {
				r.recycleMapping(m, "keepalive_timeout", now)
			}
		}
	}
}

func (r *Relay) recycleMapping(m *mapping, reason string, now time.Time) {
	r.closeMappingTransport(m)
	until := now.Add(r.cfg.RecoveryCooldown)
	r.allocator.setCooldown(m.ExternalPort, until)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.store.AddRecycle(ctx, store.Recycle{
		GatewayID: m.GatewayID, Protocol: m.Protocol, ExternalPort: m.ExternalPort,
		Reason: reason, RecycledAt: now, CooldownUntil: until,
	}); err != nil {
		// The lifecycle has already ended; retain the in-memory cooldown and log
		// for a process supervisor rather than resurrecting the mapping.
		println("persist recycle:", err.Error())
	}
	if err := r.store.DeleteMapping(ctx, m.GatewayID, m.Protocol); err != nil {
		println("delete recycled mapping:", err.Error())
	}
}

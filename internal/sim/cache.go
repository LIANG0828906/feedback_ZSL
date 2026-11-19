package sim

import (
	"sync"
	"time"

	"dnssim/internal/dnswire"
)

type cacheKey struct {
	Name  string
	Type  dnswire.Type
	Class uint16
}

type cacheEntry struct {
	stored      time.Time
	ttl         uint32
	rcode       uint8
	answers     []dnswire.RR
	authorities []dnswire.RR
	preload     bool
}

func (entry cacheEntry) expired(now time.Time) bool {
	if entry.ttl == 0 {
		return true
	}
	return now.Sub(entry.stored) >= time.Duration(entry.ttl)*time.Second
}

type dnsCache struct {
	mu      sync.Mutex
	entries map[cacheKey]cacheEntry
}

func newDNSCache() *dnsCache {
	return &dnsCache{entries: map[cacheKey]cacheEntry{}}
}

func (c *dnsCache) loadPreload(entries []PreloadEntry) {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, preload := range entries {
		t, _ := dnswire.ParseType(preload.Type)
		answers := make([]dnswire.RR, 0, len(preload.Answers))
		for _, a := range preload.Answers {
			at, _ := dnswire.ParseType(a.Type)
			rr := dnswire.RR{Name: a.Name, Type: at, Class: dnswire.ClassIN, TTL: a.TTL}
			switch at {
			case dnswire.TypeA, dnswire.TypeAAAA:
				rr.A = a.Value
			case dnswire.TypeCNAME, dnswire.TypeNS:
				rr.Target = a.Value
			case dnswire.TypeTXT:
				rr.Text = []string{a.Value}
			}
			answers = append(answers, rr)
		}
		var rcode uint8
		switch preload.RCode {
		case "NXDOMAIN":
			rcode = dnswire.RCodeNXDomain
		case "SERVFAIL":
			rcode = dnswire.RCodeServFail
		case "NODATA", "NOERROR", "":
			rcode = dnswire.RCodeNoError
		}
		key := cacheKey{Name: preload.Name, Type: t, Class: dnswire.ClassIN}
		c.entries[key] = cacheEntry{
			stored:  now,
			ttl:     preload.TTL,
			rcode:   rcode,
			answers: answers,
			preload: true,
		}
	}
}

func (c *dnsCache) get(key cacheKey, now time.Time) (handlerReply, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return handlerReply{}, false
	}
	if entry.expired(now) {
		delete(c.entries, key)
		return handlerReply{}, false
	}
	elapsed := uint32(now.Sub(entry.stored) / time.Second)
	answers := decrementRRs(entry.answers, elapsed)
	authorities := decrementRRs(entry.authorities, elapsed)
	status := "hit"
	if entry.preload {
		status = "preload"
	}
	return handlerReply{
		msg: dnswire.Message{
			Response:           true,
			RecursionAvailable: true,
			RCode:              entry.rcode,
			Answers:            answers,
			Authorities:        authorities,
		},
		cacheStatus: status,
		fromCache:   true,
	}, true
}

func decrementRRs(in []dnswire.RR, elapsed uint32) []dnswire.RR {
	out := make([]dnswire.RR, 0, len(in))
	for _, rr := range in {
		cp := rr
		if cp.TTL > elapsed {
			cp.TTL -= elapsed
		} else {
			cp.TTL = 0
		}
		cp.Text = append([]string(nil), rr.Text...)
		if rr.SOA != nil {
			soa := *rr.SOA
			cp.SOA = &soa
		}
		out = append(out, cp)
	}
	return out
}

func (c *dnsCache) put(key cacheKey, reply handlerReply, now time.Time) {
	if reply.fromCache || reply.msg.RCode == dnswire.RCodeServFail || reply.msg.RCode == dnswire.RCodeRefused {
		return
	}
	ttl := responseTTL(reply.msg)
	if ttl == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = cacheEntry{
		stored:      now,
		ttl:         ttl,
		rcode:       reply.msg.RCode,
		answers:     cloneRRs(reply.msg.Answers),
		authorities: cloneRRs(reply.msg.Authorities),
	}
}

func cloneRRs(in []dnswire.RR) []dnswire.RR {
	out := make([]dnswire.RR, 0, len(in))
	for _, rr := range in {
		cp := rr
		cp.Text = append([]string(nil), rr.Text...)
		cp.Raw = append([]byte(nil), rr.Raw...)
		if rr.SOA != nil {
			soa := *rr.SOA
			cp.SOA = &soa
		}
		out = append(out, cp)
	}
	return out
}

func responseTTL(msg dnswire.Message) uint32 {
	var ttl uint32
	for _, rr := range append(append([]dnswire.RR(nil), msg.Answers...), msg.Authorities...) {
		if rr.Type == dnswire.TypeOPT || rr.Type == dnswire.TypeSOA {
			continue
		}
		if rr.TTL == 0 {
			continue
		}
		if ttl == 0 || rr.TTL < ttl {
			ttl = rr.TTL
		}
	}
	if ttl == 0 {
		for _, rr := range msg.Authorities {
			if rr.Type == dnswire.TypeSOA && rr.SOA != nil {
				if rr.TTL > 0 {
					ttl = rr.TTL
				} else if rr.SOA.Minimum > 0 {
					ttl = rr.SOA.Minimum
				}
				break
			}
		}
	}
	return ttl
}

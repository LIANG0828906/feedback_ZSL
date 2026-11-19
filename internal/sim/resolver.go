package sim

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"dnssim/internal/dnswire"
)

var queryID atomic.Uint32

type exchangeOptions struct {
	TraceID          string
	Depth            int
	BypassCache      bool
	NoFaults         bool
	Retries          int
	RecursionDesired bool
	Deadline         time.Time
}

type exchangeResult struct {
	msg  dnswire.Message
	rtt  time.Duration
	meta ResponseMeta
}

func (e *Engine) answerRecursive(rt *resolverRuntime, q dnswire.Question, meta traceMeta, lease *slotLease) handlerReply {
	key := cacheKey{Name: q.Name, Type: q.Type, Class: q.Class}
	if !meta.Bypass {
		if cached, ok := rt.cache.get(key, time.Now()); ok {
			cached.msg.Questions = []dnswire.Question{q}
			cached.msg.RecursionAvailable = true
			cached.msg.RecursionDesired = true
			return cached
		}
	}

	retries := e.cfg.Limits.DefaultRetries
	if rt.cfg.Retries != nil {
		retries = *rt.cfg.Retries
	}
	upstream := e.resolvers[rt.cfg.Upstreams[0]]
	if upstream == nil {
		return handlerReply{
			msg:         dnswire.Message{Response: true, RCode: dnswire.RCodeServFail, Questions: []dnswire.Question{q}, Answers: []dnswire.RR{}, Authorities: []dnswire.RR{}, Additionals: []dnswire.RR{}},
			cacheStatus: "miss",
			anomaly:     "missing_upstream",
		}
	}

	var deadline time.Time
	if meta.BudgetMS > 0 {
		deadline = time.Now().
			Add(time.Duration(meta.BudgetMS) * time.Millisecond).
			Add(-20 * time.Millisecond)
	}
	lease.Suspend()
	var final exchangeResult
	var err error
	if deadline.IsZero() || time.Until(deadline) > 0 {
		final, err = e.resolveUpstream(rt, upstream, q, meta, retries, deadline)
	} else {
		err = context.DeadlineExceeded
	}
	resumeErr := lease.Resume(e.ctx)
	if err != nil {
		return handlerReply{
			msg: dnswire.Message{
				Response: true, RecursionAvailable: true, RCode: dnswire.RCodeServFail,
				Questions: []dnswire.Question{q}, Answers: []dnswire.RR{}, Authorities: []dnswire.RR{}, Additionals: []dnswire.RR{},
			},
			cacheStatus: "miss",
		}
	}
	if resumeErr != nil {
		return handlerReply{
			msg: dnswire.Message{
				Response: true, RecursionAvailable: true, RCode: dnswire.RCodeServFail,
				Questions: []dnswire.Question{q}, Answers: []dnswire.RR{}, Authorities: []dnswire.RR{}, Additionals: []dnswire.RR{},
			},
			cacheStatus: "miss",
			anomaly:     "overloaded",
		}
	}

	reply := handlerReply{
		msg: dnswire.Message{
			Response:           true,
			RecursionDesired:   true,
			RecursionAvailable: true,
			RCode:              final.msg.RCode,
			Questions:          []dnswire.Question{q},
			Answers:            cloneRRs(final.msg.Answers),
			Authorities:        cloneRRs(final.msg.Authorities),
			Additionals:        cloneRRs(final.msg.Additionals),
		},
		cacheStatus: "miss",
	}
	if meta.Bypass {
		reply.cacheStatus = "bypass"
	}
	if final.msg.RCode == dnswire.RCodeNoError || final.msg.RCode == dnswire.RCodeNXDomain {
		rt.cache.put(key, reply, time.Now())
	}
	return reply
}

func (e *Engine) resolveUpstream(rt *resolverRuntime, upstream *resolverRuntime, q dnswire.Question, meta traceMeta, retries int, deadline time.Time) (exchangeResult, error) {
	if upstream.cfg.Role == RoleForwarder || upstream.cfg.Role == RoleRecursive {
		return e.exchangeTarget(upstream.cfg.ID, upstream.cfg.Role, upstream.addr, q, exchangeOptions{
			TraceID: meta.ID, Depth: meta.Depth + 1, BypassCache: meta.Bypass, Retries: retries,
			RecursionDesired: true, Deadline: deadline,
		})
	}
	return e.resolveIterative(rt, upstream, q, meta, retries, deadline)
}

func (e *Engine) resolveIterative(rt *resolverRuntime, first *resolverRuntime, q dnswire.Question, meta traceMeta, retries int, deadline time.Time) (exchangeResult, error) {
	current := first
	depth := meta.Depth + 1
	visited := map[string]bool{}
	var last exchangeResult
	for step := 0; step < 12; step++ {
		if visited[current.cfg.ID+"|"+current.addr] {
			return last, errors.New("referral loop")
		}
		visited[current.cfg.ID+"|"+current.addr] = true
		result, err := e.exchangeTarget(current.cfg.ID, current.cfg.Role, current.addr, q, exchangeOptions{
			TraceID: meta.ID, Depth: depth, BypassCache: meta.Bypass, Retries: retries,
			RecursionDesired: false, Deadline: deadline,
		})
		if err != nil {
			return result, err
		}
		last = result
		switch result.msg.RCode {
		case dnswire.RCodeNoError:
			if len(result.msg.Answers) > 0 || len(result.msg.Authorities) > 0 && !hasNS(result.msg.Authorities) {
				return result, nil
			}
			if hasNS(result.msg.Authorities) && result.meta.NextAddress != "" {
				next := e.resolvers[result.meta.NextResolver]
				if next == nil {
					next = &resolverRuntime{cfg: Resolver{ID: "unknown", Role: "unknown", Address: result.meta.NextAddress}, addr: result.meta.NextAddress}
				}
				current = next
				depth++
				continue
			}
			return result, nil
		case dnswire.RCodeNXDomain:
			return result, nil
		default:
			return result, nil
		}
	}
	return last, errors.New("too many referrals")
}

func hasNS(records []dnswire.RR) bool {
	for _, rr := range records {
		if rr.Type == dnswire.TypeNS {
			return true
		}
	}
	return false
}

func (e *Engine) exchangeTarget(resolverID, role, address string, q dnswire.Question, opts exchangeOptions) (exchangeResult, error) {
	attempt := 1
	retries := opts.Retries
	if retries < 0 {
		retries = 0
	}
	timeout := e.resolverTimeout(resolverID)
	var lastErr error
	var last exchangeResult
	for retry := 0; retry <= retries; retry++ {
		attemptTimeout := timeout
		if !opts.Deadline.IsZero() {
			remaining := time.Until(opts.Deadline)
			if remaining <= 0 {
				return last, context.DeadlineExceeded
			}
			if remaining < attemptTimeout {
				attemptTimeout = remaining
			}
		}
		result, nextAttempt, err := e.exchangeOnce(resolverID, role, address, q, opts, attempt, attemptTimeout)
		attempt = nextAttempt
		last = result
		lastErr = err
		if err == nil {
			if result.msg.RCode == dnswire.RCodeServFail {
				continue
			}
			return result, nil
		}
		if !isTimeoutError(err) {
			return result, err
		}
	}
	return last, lastErr
}

func (e *Engine) exchangeOnce(resolverID, role, address string, q dnswire.Question, opts exchangeOptions, attempt int, timeout time.Duration) (exchangeResult, int, error) {
	started := time.Now()
	transport := "udp"
	udpResult, nextAttempt, err := e.exchangeTransport(resolverID, role, address, q, opts, attempt, transport, timeout)
	if err != nil {
		return udpResult, nextAttempt, err
	}
	if !udpResult.msg.Truncated {
		return udpResult, nextAttempt, nil
	}
	tcpTimeout := timeout - time.Since(started)
	if tcpTimeout <= 0 {
		return udpResult, nextAttempt, context.DeadlineExceeded
	}
	tcpResult, finalAttempt, err := e.exchangeTransport(resolverID, role, address, q, opts, nextAttempt, "tcp", tcpTimeout)
	return tcpResult, finalAttempt, err
}

func (e *Engine) exchangeTransport(resolverID, role, address string, q dnswire.Question, opts exchangeOptions, attempt int, transport string, timeout time.Duration) (exchangeResult, int, error) {
	hop := Hop{
		Depth:       opts.Depth,
		Resolver:    resolverID,
		Role:        role,
		Server:      address,
		Transport:   transport,
		Attempt:     attempt,
		MessageType: "query",
		RCode:       "",
		CacheStatus: "none",
		Answers:     []AnswerRecord{},
		Authority:   []AnswerRecord{},
		Additional:  []AnswerRecord{},
	}
	sequence := e.traces.append(opts.TraceID, hop)
	req := dnswire.Message{
		ID:               uint16(queryID.Add(1)),
		RecursionDesired: opts.RecursionDesired,
		Questions:        []dnswire.Question{q},
		Answers:          []dnswire.RR{},
		Authorities:      []dnswire.RR{},
		Additionals:      []dnswire.RR{},
		UDPPayloadSize:   1232,
	}
	req.AddOption(optionTraceRequest, encodeTraceMeta(traceMeta{
		ID: opts.TraceID, Depth: opts.Depth, Attempt: attempt, Bypass: opts.BypassCache, NoFaults: opts.NoFaults,
		BudgetMS: budgetMilliseconds(timeout),
	}))
	raw, err := dnswire.Pack(req)
	if err != nil {
		hop.Error = err.Error()
		hop.MessageType = "error"
		e.traces.finalize(opts.TraceID, sequence, hop)
		return exchangeResult{}, attempt, err
	}
	start := time.Now()
	responseRaw, err := wireExchange(e.ctx, address, transport, raw, timeout)
	rtt := time.Since(start)
	hop.RTTMS = roundRTT(rtt)
	if err != nil {
		hop.MessageType = "timeout"
		hop.RCode = "TIMEOUT"
		hop.TimedOut = true
		hop.Anomaly = "timeout"
		hop.Error = stableNetworkError(err)
		hop.TTL = 0
		e.traces.finalize(opts.TraceID, sequence, hop)
		return exchangeResult{rtt: rtt}, attempt + 1, err
	}
	msg, err := dnswire.Parse(responseRaw)
	if err != nil {
		hop.MessageType = "error"
		hop.RCode = "FORMERR"
		hop.Anomaly = "malformed_response"
		hop.Error = "malformed DNS response"
		e.traces.finalize(opts.TraceID, sequence, hop)
		return exchangeResult{rtt: rtt}, attempt + 1, err
	}
	if msg.ID != req.ID {
		hop.MessageType = "error"
		hop.RCode = rcodeName(msg.RCode)
		hop.Anomaly = "transaction_id_mismatch"
		hop.Error = "DNS response transaction id mismatch"
		e.traces.finalize(opts.TraceID, sequence, hop)
		return exchangeResult{rtt: rtt}, attempt + 1, errors.New("transaction id mismatch")
	}
	meta := responseMeta(msg)
	hop.MessageType = messageType(msg)
	hop.RCode = rcodeName(msg.RCode)
	hop.TTL = messageTTL(msg)
	hop.Answers = rrAnswers(msg.Answers)
	hop.Authority = rrAnswers(msg.Authorities)
	hop.Additional = rrAnswers(msg.Additionals)
	hop.CacheStatus = meta.CacheStatus
	if hop.CacheStatus == "" {
		hop.CacheStatus = "none"
	}
	hop.Truncated = msg.Truncated
	hop.Anomaly = meta.Anomaly
	if msg.Truncated {
		hop.MessageType = "truncated"
	}
	e.traces.finalize(opts.TraceID, sequence, hop)
	return exchangeResult{msg: msg, rtt: rtt, meta: meta}, attempt + 1, nil
}

func budgetMilliseconds(d time.Duration) int {
	ms := int((d + time.Millisecond - 1) / time.Millisecond)
	if ms < 0 {
		return 0
	}
	return ms
}

func (e *Engine) resolverTimeout(id string) time.Duration {
	if rt := e.resolvers[id]; rt != nil && rt.cfg.TimeoutMS > 0 {
		return time.Duration(rt.cfg.TimeoutMS) * time.Millisecond
	}
	return time.Duration(e.cfg.Limits.DefaultTimeoutMS) * time.Millisecond
}

func wireExchange(ctx context.Context, address, transport string, packet []byte, timeout time.Duration) ([]byte, error) {
	deadline := time.Now().Add(timeout)
	dialer := net.Dialer{Deadline: deadline}
	if transport == "tcp" {
		conn, err := dialer.DialContext(ctx, "tcp", address)
		if err != nil {
			return nil, err
		}
		defer conn.Close()
		_ = conn.SetDeadline(deadline)
		frame := make([]byte, 2+len(packet))
		frame[0] = byte(len(packet) >> 8)
		frame[1] = byte(len(packet))
		copy(frame[2:], packet)
		if _, err := conn.Write(frame); err != nil {
			return nil, err
		}
		var length [2]byte
		if _, err := readFull(conn, length[:]); err != nil {
			return nil, err
		}
		n := int(length[0])<<8 | int(length[1])
		if n <= 0 {
			return nil, errors.New("empty TCP DNS response")
		}
		out := make([]byte, n)
		if _, err := readFull(conn, out); err != nil {
			return nil, err
		}
		return out, nil
	}
	conn, err := dialer.DialContext(ctx, "udp", address)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline)
	if _, err := conn.Write(packet); err != nil {
		return nil, err
	}
	buf := make([]byte, 65535)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), buf[:n]...), nil
}

func readFull(conn net.Conn, data []byte) (int, error) {
	total := 0
	for total < len(data) {
		n, err := conn.Read(data[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded)
}

func stableNetworkError(err error) string {
	if isTimeoutError(err) {
		return "no response before timeout"
	}
	return "network exchange failed"
}

func messageType(msg dnswire.Message) string {
	if msg.Truncated {
		return "truncated"
	}
	switch msg.RCode {
	case dnswire.RCodeNXDomain:
		return "nxdomain"
	case dnswire.RCodeServFail:
		return "servfail"
	case dnswire.RCodeRefused:
		return "refused"
	}
	if len(msg.Answers) > 0 {
		for _, rr := range msg.Answers {
			if rr.Type == dnswire.TypeCNAME {
				return "cname"
			}
		}
		return "answer"
	}
	if hasNS(msg.Authorities) {
		return "referral"
	}
	return "nodata"
}

func messageTTL(msg dnswire.Message) uint32 {
	return responseTTL(msg)
}

func rrAnswers(records []dnswire.RR) []AnswerRecord {
	out := make([]AnswerRecord, 0, len(records))
	for _, rr := range records {
		out = append(out, AnswerRecord{
			Name:  rr.Name,
			Type:  rr.Type.String(),
			TTL:   rr.TTL,
			Value: rrValue(rr),
		})
	}
	return out
}

func rrValue(rr dnswire.RR) string {
	switch rr.Type {
	case dnswire.TypeA, dnswire.TypeAAAA:
		return rr.A
	case dnswire.TypeCNAME, dnswire.TypeNS:
		return rr.Target
	case dnswire.TypeTXT:
		return strings.Join(rr.Text, "")
	case dnswire.TypeSOA:
		if rr.SOA != nil {
			return fmt.Sprintf("%s %s %d %d %d %d %d", rr.SOA.MName, rr.SOA.RName, rr.SOA.Serial, rr.SOA.Refresh, rr.SOA.Retry, rr.SOA.Expire, rr.SOA.Minimum)
		}
	}
	return ""
}

package sim

import (
	"context"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dnssim/internal/dnswire"
)

type TraceRequest struct {
	Site        string `json:"site"`
	Domain      string `json:"domain"`
	Type        string `json:"type,omitempty"`
	BypassCache bool   `json:"bypass_cache,omitempty"`
}

type traceGate struct {
	slots     chan struct{}
	queue     chan struct{}
	queueWait time.Duration
	active    atomic.Int64
	queued    atomic.Int64
}

type Service struct {
	engine    *Engine
	gate      *traceGate
	startedAt time.Time

	mappingMu sync.RWMutex
	mappings  map[string]map[string]string
}

func NewService(cfg *Config) (*Service, error) {
	engine, err := StartEngine(cfg)
	if err != nil {
		return nil, err
	}
	queueSize := cfg.Limits.TraceQueueSize
	s := &Service{
		engine:    engine,
		startedAt: time.Now(),
		mappings:  map[string]map[string]string{},
		gate: &traceGate{
			slots:     make(chan struct{}, cfg.Limits.MaxConcurrentTraces),
			queue:     make(chan struct{}, queueSize),
			queueWait: time.Duration(cfg.Limits.TraceQueueTimeoutMS) * time.Millisecond,
		},
	}
	for i := range cfg.Sites {
		site := cfg.Sites[i]
		m := make(map[string]string, len(site.StaticMappings))
		for name, value := range site.StaticMappings {
			m[name] = value
		}
		s.mappings[site.ID] = m
	}
	return s, nil
}

func (s *Service) Close() {
	s.engine.Close()
}

func (s *Service) Engine() *Engine { return s.engine }

func (s *Service) Stats() map[string]any {
	stats := s.engine.Stats()
	stats["active_traces"] = s.gate.active.Load()
	stats["queued_traces"] = s.gate.queued.Load()
	stats["trace_queue_size"] = cap(s.gate.queue)
	return stats
}

func (g *traceGate) acquire(ctx context.Context) error {
	select {
	case g.slots <- struct{}{}:
		g.active.Add(1)
		return nil
	default:
	}
	select {
	case g.queue <- struct{}{}:
		g.queued.Add(1)
		defer func() {
			<-g.queue
			g.queued.Add(-1)
		}()
	default:
		return bizError(CodeBusy, "trace", "trace queue is full")
	}
	timer := time.NewTimer(g.queueWait)
	defer timer.Stop()
	select {
	case g.slots <- struct{}{}:
		g.active.Add(1)
		return nil
	case <-ctx.Done():
		return bizError(CodeShuttingDown, "trace", "service is shutting down")
	case <-timer.C:
		return bizError(CodeQueueTimeout, "trace", "timed out waiting for a trace slot")
	}
}

func (g *traceGate) release() {
	select {
	case <-g.slots:
		g.active.Add(-1)
	default:
	}
}

func (s *Service) Trace(ctx context.Context, request TraceRequest) (TraceResult, error) {
	if err := s.gate.acquire(ctx); err != nil {
		return TraceResult{}, err
	}
	defer s.gate.release()

	siteID := strings.TrimSpace(request.Site)
	if siteID == "" {
		return TraceResult{}, bizError(CodeInvalidRequest, "site", "site is required")
	}
	site, ok := s.engine.site(siteID)
	if !ok {
		return TraceResult{}, bizError(CodeUnknownSite, "site", "unknown site %q", request.Site)
	}
	domain, err := dnswire.CanonicalName(request.Domain)
	if err != nil {
		return TraceResult{}, bizError(CodeInvalidRequest, "domain", "%v", err)
	}
	qtype, err := dnswire.ParseType(request.Type)
	if err != nil {
		return TraceResult{}, bizError(CodeInvalidType, "type", "%v", err)
	}
	if qtype == dnswire.TypeANY {
		return TraceResult{}, bizError(CodeInvalidType, "type", "ANY queries are not traceable; choose A, AAAA, CNAME, NS, or TXT")
	}
	q := dnswire.Question{Name: domain, Type: qtype, Class: dnswire.ClassIN}
	rec := s.engine.resolvers[site.Recursive]
	if rec == nil {
		return TraceResult{}, bizError(CodeInternal, "site.recursive", "recursive resolver is unavailable")
	}

	id, session := s.engine.traces.open()
	defer s.engine.traces.close(id)

	mainSource := "dns"
	var mainResult exchangeResult
	var mainErr error
	var localHop *Hop
	var localSeq int

	s.mappingMu.RLock()
	staticValue, hasStatic := s.mappings[site.ID][domain]
	if qtype != dnswire.TypeA && qtype != dnswire.TypeAAAA {
		hasStatic = false
	}
	if hasStatic {
		ip := net.ParseIP(staticValue)
		if ip == nil || (qtype == dnswire.TypeA && ip.To4() == nil) || (qtype == dnswire.TypeAAAA && ip.To4() != nil) {
			hasStatic = false
		}
	}
	s.mappingMu.RUnlock()
	negative, hasNegative := s.findNegativeCache(site, domain, qtype, time.Now())

	switch {
	case hasStatic:
		mainSource = "hosts"
		localHop = &Hop{
			Depth:       0,
			Resolver:    "hosts",
			Role:        "hosts",
			Server:      "local",
			Transport:   "local",
			Attempt:     1,
			MessageType: "answer",
			RCode:       "NOERROR",
			TTL:         0,
			CacheStatus: "static",
			Answers: []AnswerRecord{{
				Name: domain, Type: qtype.String(), TTL: 0, Value: staticValue,
			}},
			Authority:  []AnswerRecord{},
			Additional: []AnswerRecord{},
		}
		hop := *localHop
		localSeq = s.engine.traces.append(id, hop)
	case hasNegative:
		mainSource = "negative_cache"
		status := "NODATA"
		rcode := "NOERROR"
		if negative.RCode == "NXDOMAIN" {
			status = "NXDOMAIN"
			rcode = "NXDOMAIN"
		}
		localHop = &Hop{
			Depth:       0,
			Resolver:    "negative-cache",
			Role:        "negative_cache",
			Server:      "local",
			Transport:   "local",
			Attempt:     1,
			MessageType: strings.ToLower(status),
			RCode:       rcode,
			TTL:         negative.TTL,
			CacheStatus: "negative_cache",
			Answers:     []AnswerRecord{},
			Authority:   []AnswerRecord{},
			Additional:  []AnswerRecord{},
		}
		hop := *localHop
		localSeq = s.engine.traces.append(id, hop)
	default:
		retries := s.engine.cfg.Limits.DefaultRetries
		if site.Retries != nil {
			retries = *site.Retries
		}
		mainResult, mainErr = s.engine.exchangeTarget(rec.cfg.ID, rec.cfg.Role, rec.addr, q, exchangeOptions{
			TraceID:          id,
			Depth:            0,
			BypassCache:      request.BypassCache,
			Retries:          retries,
			RecursionDesired: true,
		})
	}

	authoritative := s.engine.authoritativeView(q)
	result := TraceResult{
		SchemaVersion: 1,
		Site:          site.ID,
		Domain:        domain,
		QueryType:     qtype.String(),
		MismatchCause: "none",
	}

	switch mainSource {
	case "hosts":
		result.Resolution = FinalResolution{
			Status:  "SUCCESS",
			RCode:   "NOERROR",
			TTL:     0,
			Source:  "hosts",
			Values:  []string{staticValue},
			Answers: []AnswerRecord{{Name: domain, Type: qtype.String(), TTL: 0, Value: staticValue}},
		}
	case "negative_cache":
		status := "NODATA"
		rcode := "NOERROR"
		if negative.RCode == "NXDOMAIN" {
			status = "NXDOMAIN"
			rcode = "NXDOMAIN"
		}
		result.Resolution = FinalResolution{
			Status: status, RCode: rcode, TTL: negative.TTL, Source: "negative_cache",
			Values: []string{}, Answers: []AnswerRecord{},
		}
	default:
		result.Resolution = resolutionFromExchange(mainResult, mainErr, request.BypassCache, qtype)
	}

	result.Authoritative = authoritative
	consistent, cause := compareResolution(result.Resolution, authoritative)
	if cause == "answer_mismatch" && hasCacheHop(session) {
		cause = "cache_poisoning"
	}
	result.ConsistentWithAuthority = consistent
	result.MismatchCause = cause

	if !consistent {
		switch {
		case mainSource == "hosts":
			if localHop != nil {
				localHop.Anomaly = "static_mapping"
				s.engine.traces.finalize(id, localSeq, *localHop)
			}
		case mainSource == "negative_cache":
			if localHop != nil {
				localHop.Anomaly = "negative_cache"
				s.engine.traces.finalize(id, localSeq, *localHop)
			}
		case cause == "cache_poisoning":
			markCachePoisoning(session, id, s.engine.traces)
		case cause == "authority_unavailable", cause == "link_fault":
		default:
			if cause == "answer_mismatch" {
				markFirstAnswerMismatch(session, id, s.engine.traces)
			}
		}
	}

	hops := session.snapshot()
	result.Hops = hops
	if result.MismatchCause == "authority_unavailable" {
		switch result.Resolution.Status {
		case "TIMEOUT":
			result.Conclusion = "TIMEOUT"
		case "SERVFAIL":
			result.Conclusion = "SERVFAIL"
		case "ERROR":
			result.Conclusion = "ERROR"
		case "NXDOMAIN":
			result.Conclusion = "NXDOMAIN"
		default:
			result.Conclusion = "SUCCESS"
		}
	} else {
		result.Conclusion = finalConclusion(result.Resolution, consistent)
	}
	attribution := earliestAnomaly(hops, result.Conclusion)
	result.Attribution = attribution
	if attribution != nil {
		attribution.Conclusion = result.Conclusion
	}
	normalizeResultSlices(&result)
	return result, nil
}

func (s *Service) findNegativeCache(site *Site, name string, qtype dnswire.Type, now time.Time) (NegativeCacheEntry, bool) {
	for _, entry := range site.NegativeCache {
		if entry.Name == name && entry.Type == qtype.String() {
			elapsed := uint32(now.Sub(s.startedAt) / time.Second)
			if elapsed >= entry.TTL {
				continue
			}
			entry.TTL -= elapsed
			return entry, true
		}
	}
	return NegativeCacheEntry{}, false
}

func resolutionFromExchange(result exchangeResult, err error, bypass bool, qtype dnswire.Type) FinalResolution {
	source := "dns"
	if bypass {
		source = "dns_bypass"
	}
	if err != nil {
		status := "TIMEOUT"
		rcode := "TIMEOUT"
		if !isTimeoutError(err) {
			status = "ERROR"
			rcode = "ERROR"
		}
		return FinalResolution{Status: status, RCode: rcode, TTL: 0, Source: source, Values: []string{}, Answers: []AnswerRecord{}}
	}
	msg := result.msg
	status := statusFromMessage(msg)
	return FinalResolution{
		Status:  status,
		RCode:   rcodeName(msg.RCode),
		TTL:     messageTTL(msg),
		Source:  source,
		Values:  valuesFromRRs(msg.Answers, qtype),
		Answers: rrAnswers(msg.Answers),
	}
}

func (e *Engine) authoritativeView(q dnswire.Question) *AuthoritativeView {
	rt, _, ok := e.authorityFor(q.Name)
	if !ok {
		return &AuthoritativeView{
			Available: true,
			Status:    "NXDOMAIN",
			RCode:     "NXDOMAIN",
			TTL:       0,
			Answers:   []AnswerRecord{},
			Values:    []string{},
		}
	}
	result, err := e.exchangeTarget(rt.cfg.ID, rt.cfg.Role, rt.addr, q, exchangeOptions{
		BypassCache:      true,
		NoFaults:         true,
		Retries:          0,
		RecursionDesired: false,
	})
	if err != nil {
		return &AuthoritativeView{
			Available: false,
			Status:    "UNAVAILABLE",
			RCode:     "UNAVAILABLE",
			TTL:       0,
			Answers:   []AnswerRecord{},
			Values:    []string{},
		}
	}
	msg := result.msg
	return &AuthoritativeView{
		Available: true,
		Status:    statusFromMessage(msg),
		RCode:     rcodeName(msg.RCode),
		TTL:       messageTTL(msg),
		Answers:   rrAnswers(msg.Answers),
		Values:    valuesFromRRs(msg.Answers, q.Type),
	}
}

func statusFromMessage(msg dnswire.Message) string {
	switch msg.RCode {
	case dnswire.RCodeNXDomain:
		return "NXDOMAIN"
	case dnswire.RCodeServFail:
		return "SERVFAIL"
	case dnswire.RCodeRefused:
		return "ERROR"
	case dnswire.RCodeNoError:
		if len(msg.Answers) > 0 {
			return "SUCCESS"
		}
		return "NODATA"
	default:
		return "ERROR"
	}
}

func compareResolution(main FinalResolution, auth *AuthoritativeView) (bool, string) {
	if auth == nil || !auth.Available {
		return false, "authority_unavailable"
	}
	if main.Status == "TIMEOUT" || main.Status == "SERVFAIL" || main.Status == "ERROR" {
		return false, "link_fault"
	}
	if main.Source == "hosts" {
		if auth.Status != "SUCCESS" || len(main.Values) != 1 {
			return false, "static_mapping"
		}
		authValues := sortedValues(auth.Values)
		if len(authValues) == 1 && authValues[0] == main.Values[0] {
			return true, "none"
		}
		return false, "static_mapping"
	}
	if main.Source == "negative_cache" {
		if main.Status == "NXDOMAIN" && auth.Status == "NXDOMAIN" {
			return true, "none"
		}
		if main.Status == "NODATA" && (auth.Status == "NODATA" || auth.Status == "NXDOMAIN") {
			return true, "none"
		}
		return false, "negative_cache"
	}
	switch {
	case main.Status == "NXDOMAIN" && auth.Status == "NXDOMAIN":
		return true, "none"
	case main.Status == "NODATA" && auth.Status == "NODATA":
		return true, "none"
	case main.Status == "SUCCESS" && auth.Status == "SUCCESS":
		if answerSetsEqual(main.Answers, auth.Answers) {
			return true, "none"
		}
		return false, "answer_mismatch"
	case main.Status == "SUCCESS" && auth.Status != "SUCCESS":
		return false, "answer_mismatch"
	case main.Status == "NXDOMAIN" || main.Status == "NODATA":
		return false, "answer_mismatch"
	default:
		return false, "answer_mismatch"
	}
}

func hasCacheHop(session *traceSession) bool {
	for _, hop := range session.snapshot() {
		if hop.CacheStatus == "preload" || hop.CacheStatus == "hit" {
			return true
		}
	}
	return false
}

func answerSetsEqual(left, right []AnswerRecord) bool {
	return canonicalAnswers(left) == canonicalAnswers(right)
}

func canonicalAnswers(records []AnswerRecord) string {
	a := append([]AnswerRecord(nil), records...)
	sort.SliceStable(a, func(i, j int) bool {
		if a[i].Name != a[j].Name {
			return a[i].Name < a[j].Name
		}
		if a[i].Type != a[j].Type {
			return a[i].Type < a[j].Type
		}
		return a[i].Value < a[j].Value
	})
	var b strings.Builder
	for _, answer := range a {
		b.WriteString(answer.Name)
		b.WriteByte('|')
		b.WriteString(answer.Type)
		b.WriteByte('|')
		b.WriteString(answer.Value)
		b.WriteByte('\n')
	}
	return b.String()
}

func valuesFromRRs(records []dnswire.RR, qtype dnswire.Type) []string {
	var values []string
	for _, rr := range records {
		if rr.Type == qtype || qtype == dnswire.TypeA && rr.Type == dnswire.TypeA || qtype == dnswire.TypeAAAA && rr.Type == dnswire.TypeAAAA {
			values = append(values, rrValue(rr))
		}
	}
	if len(values) == 0 {
		for _, rr := range records {
			switch rr.Type {
			case dnswire.TypeA, dnswire.TypeAAAA, dnswire.TypeCNAME, dnswire.TypeNS, dnswire.TypeTXT:
				values = append(values, rrValue(rr))
			}
		}
	}
	return sortedValues(values)
}

func sortedValues(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

func finalConclusion(resolution FinalResolution, consistent bool) string {
	switch resolution.Status {
	case "TIMEOUT":
		return "TIMEOUT"
	case "SERVFAIL":
		return "SERVFAIL"
	case "ERROR":
		return "ERROR"
	}
	if !consistent {
		return "INCONSISTENT"
	}
	switch resolution.Status {
	case "NXDOMAIN":
		return "NXDOMAIN"
	default:
		return "SUCCESS"
	}
}

func earliestAnomaly(hops []Hop, conclusion string) *Attribution {
	candidates := make([]*Hop, 0, len(hops))
	for i := range hops {
		hop := &hops[i]
		if hop.Anomaly == "" {
			continue
		}
		if (hop.Role == RoleRecursive || hop.Role == RoleForwarder) && hop.Anomaly == "timeout" {
			propagated := false
			for j := range hops {
				if hops[j].Depth > hop.Depth && hops[j].Anomaly != "" {
					propagated = true
					break
				}
			}
			if propagated {
				continue
			}
		}
		candidates = append(candidates, hop)
	}
	var selected *Hop
	for _, hop := range candidates {
		if selected == nil || hop.Sequence < selected.Sequence {
			selected = hop
		}
	}
	if selected == nil {
		return nil
	}
	return &Attribution{
		Sequence:   selected.Sequence,
		Resolver:   selected.Resolver,
		Role:       selected.Role,
		Server:     selected.Server,
		Anomaly:    selected.Anomaly,
		Conclusion: conclusion,
		Detail:     anomalyDetail(selected.Anomaly),
	}
}

func anomalyDetail(anomaly string) string {
	switch anomaly {
	case "timeout":
		return "no response before timeout"
	case "servfail":
		return "server returned SERVFAIL"
	case "wrong_delegation":
		return "delegation points to an unexpected resolver"
	case "overloaded":
		return "server query concurrency limit was reached"
	case "static_mapping":
		return "static hosts mapping differs from the authoritative answer"
	case "negative_cache":
		return "preloaded negative cache masks an authoritative answer"
	case "cache_poisoning":
		return "cached answer differs from the authoritative answer"
	case "answer_mismatch":
		return "DNS answer differs from the authoritative answer"
	case "referral_loop":
		return "referral chain contains a loop"
	case "invalid_delegation_address", "missing_delegation_target":
		return "referral has no usable target"
	case "malformed_response":
		return "response could not be decoded"
	case "transaction_id_mismatch":
		return "response transaction id differs from the query"
	default:
		return anomaly
	}
}

func markCachePoisoning(session *traceSession, id string, collector *traceCollector) {
	hops := session.snapshot()
	for i := range hops {
		if hops[i].CacheStatus == "preload" || hops[i].CacheStatus == "hit" {
			hops[i].Anomaly = "cache_poisoning"
			collector.finalize(id, hops[i].Sequence, hops[i])
			return
		}
	}
}

func markFirstAnswerMismatch(session *traceSession, id string, collector *traceCollector) {
	hops := session.snapshot()
	for i := range hops {
		if hops[i].Depth == 0 {
			hops[i].Anomaly = "answer_mismatch"
			collector.finalize(id, hops[i].Sequence, hops[i])
			return
		}
	}
}

func normalizeResultSlices(result *TraceResult) {
	if result.Resolution.Values == nil {
		result.Resolution.Values = []string{}
	}
	if result.Resolution.Answers == nil {
		result.Resolution.Answers = []AnswerRecord{}
	}
	if result.Hops == nil {
		result.Hops = []Hop{}
	}
	for i := range result.Hops {
		if result.Hops[i].Answers == nil {
			result.Hops[i].Answers = []AnswerRecord{}
		}
		if result.Hops[i].Authority == nil {
			result.Hops[i].Authority = []AnswerRecord{}
		}
		if result.Hops[i].Additional == nil {
			result.Hops[i].Additional = []AnswerRecord{}
		}
	}
}

func (s *Service) SetStaticMapping(siteID, name, value string) error {
	if _, ok := s.engine.site(siteID); !ok {
		return bizError(CodeUnknownSite, "site", "unknown site %q", siteID)
	}
	canonical, err := dnswire.CanonicalName(name)
	if err != nil {
		return bizError(CodeInvalidRequest, "name", "%v", err)
	}
	value = strings.TrimSpace(value)
	if !validIP(value) {
		return bizError(CodeInvalidRequest, "value", "static mapping value must be an IP address")
	}
	s.mappingMu.Lock()
	defer s.mappingMu.Unlock()
	if s.mappings[siteID] == nil {
		s.mappings[siteID] = map[string]string{}
	}
	s.mappings[siteID][canonical] = value
	return nil
}

func (s *Service) DeleteStaticMapping(siteID, name string) (bool, error) {
	if _, ok := s.engine.site(siteID); !ok {
		return false, bizError(CodeUnknownSite, "site", "unknown site %q", siteID)
	}
	canonical, err := dnswire.CanonicalName(name)
	if err != nil {
		return false, bizError(CodeInvalidRequest, "name", "%v", err)
	}
	s.mappingMu.Lock()
	defer s.mappingMu.Unlock()
	_, existed := s.mappings[siteID][canonical]
	delete(s.mappings[siteID], canonical)
	return existed, nil
}

func (s *Service) ClearStaticMappings(siteID string) (int, error) {
	if _, ok := s.engine.site(siteID); !ok {
		return 0, bizError(CodeUnknownSite, "site", "unknown site %q", siteID)
	}
	s.mappingMu.Lock()
	defer s.mappingMu.Unlock()
	n := len(s.mappings[siteID])
	s.mappings[siteID] = map[string]string{}
	return n, nil
}

func (s *Service) StaticMappings(siteID string) (map[string]string, error) {
	if _, ok := s.engine.site(siteID); !ok {
		return nil, bizError(CodeUnknownSite, "site", "unknown site %q", siteID)
	}
	s.mappingMu.RLock()
	defer s.mappingMu.RUnlock()
	out := make(map[string]string, len(s.mappings[siteID]))
	for name, value := range s.mappings[siteID] {
		out[name] = value
	}
	return out, nil
}

func validIP(value string) bool {
	return len(strings.TrimSpace(value)) > 0 && net.ParseIP(strings.TrimSpace(value)) != nil
}

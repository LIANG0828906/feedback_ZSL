package sample

import (
	"fmt"
	"math/rand"
	"strconv"

	"samplingkernel/internal/model"
)

const DefaultSeed int64 = 20261126

type Bundle struct {
	Project *model.Project
	Docs    map[string][]byte
	Truth   *model.Report
}

type builder struct {
	project model.Project
	seq     int
}

func Generate() (*Bundle, error) {
	return GenerateSeed(DefaultSeed)
}

func GenerateSeed(seed int64) (*Bundle, error) {
	b := &builder{}
	b.project = model.Project{
		ProjectID:        "sample-checkout",
		Seed:             seed,
		ClockOriginMS:    1764100800000,
		MillisPerSeqStep: 25,
		Budget: model.BudgetPolicy{
			WindowMS:             1000,
			GlobalLimitSpans:     10,
			GlobalExhaustionRule: model.GlobalExhaustReject,
		},
		OrphanPolicy: model.OrphanQuarantine,
		Services: []model.Service{
			{ID: "gw", Name: "gateway"},
			{ID: "ord", Name: "order"},
			{ID: "pay", Name: "payment"},
			{ID: "inv", Name: "inventory"},
			{ID: "risk", Name: "risk"},
		},
		Edges: []model.Edge{
			{No: 1, ID: "e-checkout", FromServiceID: "gw", ToServiceID: "ord", BaseRatePermyriad: 3500, TailRuleIDs: []string{"tr-head", "tr-latency"}},
			{No: 2, ID: "e-pay", FromServiceID: "ord", ToServiceID: "pay", BaseRatePermyriad: 10000, TailRuleIDs: []string{"tr-error"}},
			{No: 3, ID: "e-inventory", FromServiceID: "ord", ToServiceID: "inv", BaseRatePermyriad: 6000, TailRuleIDs: []string{"tr-spans"}},
			{No: 4, ID: "e-risk", FromServiceID: "ord", ToServiceID: "risk", BaseRatePermyriad: 2500, ForcedRetention: true, TailRuleIDs: []string{"tr-spans"}},
			{No: 5, ID: "e-readonly", FromServiceID: "gw", ToServiceID: "ord", BaseRatePermyriad: 0, TailRuleIDs: []string{"tr-head"}},
			{No: 6, ID: "e-full", FromServiceID: "ord", ToServiceID: "pay", BaseRatePermyriad: 10000, TailRuleIDs: []string{"tr-head"}},
		},
		Allocations: []model.Allocation{
			{ServiceID: "gw", WeightPermyriad: 3000},
			{ServiceID: "ord", WeightPermyriad: 2000},
			{ServiceID: "pay", WeightPermyriad: 2000},
			{ServiceID: "inv", WeightPermyriad: 2000},
			{ServiceID: "risk", WeightPermyriad: 1000},
		},
		TailRules: []model.TailRule{
			{ID: "tr-error", Priority: 10, Order: 1, EdgeID: "e-pay", Condition: model.TailCondition{Kind: "span_status", Value: "error"}},
			{ID: "tr-latency", Priority: 20, Order: 2, EdgeID: "e-checkout", Condition: model.TailCondition{Kind: "latency_ms", Op: "gt", Value: "80"}},
			{ID: "tr-head", Priority: 30, Order: 3, EdgeID: "e-checkout", Condition: model.TailCondition{Kind: "head_sampled", Op: "eq", Value: "true"}},
			{ID: "tr-spans", Priority: 40, Order: 4, EdgeID: "e-inventory", Condition: model.TailCondition{Kind: "span_count", Op: "gte", Value: "5"}},
		},
	}

	b.buildEvents()
	rng := rand.New(rand.NewSource(seed))
	for i := range b.project.Events {
		span := b.project.Events[i].Span
		if span == nil {
			continue
		}
		if span.Tags == nil {
			span.Tags = map[string]string{}
		}
		span.Tags["generator.seed"] = strconv.FormatInt(seed, 10)
		span.Tags["generator.draw"] = strconv.Itoa(rng.Intn(1_000_000))
	}
	truth := recordTruth(&b.project)
	docs, err := renderDocs(&b.project)
	if err != nil {
		return nil, err
	}
	return &Bundle{Project: &b.project, Docs: docs, Truth: truth}, nil
}

func (b *builder) next(kind model.EventKind) model.Event {
	b.seq++
	return model.Event{
		Seq:         b.seq,
		Kind:        kind,
		TimestampMS: b.project.ClockOriginMS + int64(b.seq-1)*b.project.MillisPerSeqStep,
	}
}

func (b *builder) enter(requestID, traceID, edgeID string) {
	event := b.next(model.EventRequestEnter)
	event.Enter = &model.EnterPayload{RequestID: requestID, TraceID: traceID, EdgeID: edgeID}
	b.project.Events = append(b.project.Events, event)
}

func (b *builder) propagate(requestID, traceID, edgeID string) {
	event := b.next(model.EventPropagate)
	event.Propagate = &model.EdgePayload{RequestID: requestID, TraceID: traceID, EdgeID: edgeID}
	b.project.Events = append(b.project.Events, event)
}

func (b *builder) span(sp model.Span) {
	event := b.next(model.EventSpanReport)
	sp.ArrivalSeq = event.Seq
	event.Span = &sp
	b.project.Events = append(b.project.Events, event)
}

func (b *builder) settle(traceID, rootSpanID, serviceID string) {
	event := b.next(model.EventRootSettle)
	event.Settle = &model.SettlePayload{TraceID: traceID, RootSpanID: rootSpanID, ServiceID: serviceID}
	b.project.Events = append(b.project.Events, event)
}

func baseSpan(trace, id, parent, service string, causal int, latency uint64, status string) model.Span {
	if status == "" {
		status = "ok"
	}
	return model.Span{
		SpanID: id, TraceID: trace, ParentSpanID: parent, ServiceID: service,
		CausalOrder: causal, Name: fmt.Sprintf("%s-operation", service),
		Status: status, LatencyMS: latency, Tags: map[string]string{},
	}
}

func (b *builder) buildEvents() {
	b.trace1001()
	b.trace1002()
	b.trace1003()
	b.trace1004()
	b.trace1005()
	b.unknownSpan()
	b.trace1006()
	b.trace1007()
	b.trace1008()
	b.boundaryTraces()
}

func (b *builder) trace1001() {
	const trace = "tr-1001"
	b.enter("req-1001", trace, "e-checkout")
	b.propagate("req-1001", trace, "e-pay")
	pay := baseSpan(trace, "sp-1001-pay", "sp-1001-ord", "pay", 3, 18, "error")
	pay.EdgeID = "e-pay"
	pay.Tags["error.type"] = "payment_declined"
	b.span(pay)
	inv := baseSpan(trace, "sp-1001-inv", "sp-1001-ord", "inv", 4, 12, "ok")
	inv.EdgeID = "e-inventory"
	b.span(inv)
	ord := baseSpan(trace, "sp-1001-ord", "sp-1001-gw", "ord", 2, 42, "ok")
	b.span(ord)
	gw := baseSpan(trace, "sp-1001-gw", "", "gw", 1, 124, "ok")
	b.span(gw)
	b.span(pay)
	b.settle(trace, "sp-1001-gw", "gw")
}

func (b *builder) trace1002() {
	const trace = "tr-1002"
	b.enter("req-1002", trace, "e-checkout")
	b.propagate("req-1002", trace, "e-inventory")
	orphan := baseSpan(trace, "sp-1002-orphan-risk", "sp-1002-missing-parent", "risk", 9, 11, "ok")
	b.span(orphan)
	inv := baseSpan(trace, "sp-1002-inv", "sp-1002-ord", "inv", 4, 16, "ok")
	b.span(inv)
	pay := baseSpan(trace, "sp-1002-pay", "sp-1002-ord", "pay", 3, 21, "ok")
	b.span(pay)
	ord := baseSpan(trace, "sp-1002-ord", "sp-1002-gw", "ord", 2, 96, "ok")
	b.span(ord)
	gw := baseSpan(trace, "sp-1002-gw", "", "gw", 1, 31, "ok")
	b.span(gw)
	b.span(gw)
	b.settle(trace, "sp-1002-gw", "gw")
}

func (b *builder) trace1003() {
	const trace = "tr-1003"
	b.enter("req-1003", trace, "e-checkout")
	inv := baseSpan(trace, "sp-1003-inv", "sp-1003-ord", "inv", 3, 14, "ok")
	b.span(inv)
	ord := baseSpan(trace, "sp-1003-ord", "sp-1003-gw", "ord", 2, 22, "ok")
	b.span(ord)
	gw := baseSpan(trace, "sp-1003-gw", "", "gw", 1, 28, "ok")
	b.span(gw)
	b.settle(trace, "sp-1003-gw", "gw")
}

func (b *builder) trace1004() {
	const trace = "tr-1004"
	b.enter("req-1004", trace, "e-checkout")
	b.propagate("req-1004", trace, "e-risk")
	risk := baseSpan(trace, "sp-1004-risk", "sp-1004-ord", "risk", 3, 8, "ok")
	risk.EdgeID = "e-risk"
	risk.Tags["force.marker"] = "case-review"
	b.span(risk)
	ord := baseSpan(trace, "sp-1004-ord", "sp-1004-gw", "ord", 2, 19, "ok")
	b.span(ord)
	gw := baseSpan(trace, "sp-1004-gw", "", "gw", 1, 25, "ok")
	b.span(gw)
	b.settle(trace, "sp-1004-gw", "gw")
}

func (b *builder) trace1005() {
	const trace = "tr-1005"
	b.enter("req-1005", trace, "e-checkout")
	pay := baseSpan(trace, "sp-1005-pay", "sp-1005-ord", "pay", 3, 15, "ok")
	b.span(pay)
	ord := baseSpan(trace, "sp-1005-ord", "sp-1005-gw", "ord", 2, 20, "ok")
	b.span(ord)
	gw := baseSpan(trace, "sp-1005-gw", "", "gw", 1, 26, "ok")
	b.span(gw)
	b.settle(trace, "sp-1005-gw", "gw")
}

func (b *builder) unknownSpan() {
	sp := baseSpan("tr-unknown", "sp-unknown-0001", "sp-missing-0001", "gw", 1, 3, "ok")
	b.span(sp)
}

func (b *builder) trace1006() {
	const trace = "tr-1006"
	b.enter("req-1006", trace, "e-checkout")
	pay1 := baseSpan(trace, "sp-1006-pay-1", "sp-1006-ord", "pay", 3, 12, "ok")
	b.span(pay1)
	pay2 := baseSpan(trace, "sp-1006-pay-2", "sp-1006-ord", "pay", 4, 16, "ok")
	b.span(pay2)
	ord := baseSpan(trace, "sp-1006-ord", "sp-1006-gw", "ord", 2, 30, "ok")
	b.span(ord)
	gw := baseSpan(trace, "sp-1006-gw", "", "gw", 1, 35, "ok")
	b.span(gw)
	b.propagate("req-1006", trace, "e-pay")
	b.settle(trace, "sp-1006-gw", "gw")
}

func (b *builder) trace1007() {
	const trace = "tr-1007"
	b.enter("req-1007", trace, "e-checkout")
	inv := baseSpan(trace, "sp-1007-inv", "sp-1007-ord", "inv", 4, 11, "ok")
	b.span(inv)
	pay := baseSpan(trace, "sp-1007-pay", "sp-1007-ord", "pay", 3, 13, "ok")
	b.span(pay)
	ord := baseSpan(trace, "sp-1007-ord", "sp-1007-gw", "ord", 2, 91, "ok")
	b.span(ord)
	gw := baseSpan(trace, "sp-1007-gw", "", "gw", 1, 22, "ok")
	b.span(gw)
	b.settle(trace, "sp-1007-gw", "gw")
}

func (b *builder) trace1008() {
	const trace = "tr-1008"
	b.enter("req-1008", trace, "e-checkout")
	inv := baseSpan(trace, "sp-1008-inv", "sp-1008-ord", "inv", 3, 17, "ok")
	b.span(inv)
	ord := baseSpan(trace, "sp-1008-ord", "sp-1008-gw", "ord", 2, 24, "ok")
	b.span(ord)
	gw := baseSpan(trace, "sp-1008-gw", "", "gw", 1, 29, "ok")
	b.span(gw)
	b.settle(trace, "sp-1008-gw", "gw")
}

func (b *builder) boundaryTraces() {
	b.enter("req-zero", "tr-zero", "e-readonly")
	gw0 := baseSpan("tr-zero", "sp-zero-gw", "", "gw", 1, 5, "ok")
	b.span(gw0)
	b.settle("tr-zero", "sp-zero-gw", "gw")

	b.enter("req-full", "tr-full", "e-full")
	gwFull := baseSpan("tr-full", "sp-full-gw", "", "gw", 1, 6, "ok")
	b.span(gwFull)
	b.settle("tr-full", "sp-full-gw", "gw")
}

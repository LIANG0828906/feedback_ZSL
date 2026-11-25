package gateway

import (
	"sort"
)

type EditKind string

const (
	EditErrorRule    EditKind = "error_rule"
	EditWindowConfig EditKind = "window_config"
	EditLateness     EditKind = "allowed_lateness"
	EditAddRecord    EditKind = "add_record"
	EditUpdateRecord EditKind = "update_record"
	EditDeleteRecord EditKind = "delete_record"
	EditWatermark    EditKind = "watermark"
)

type Edit struct {
	Kind              EditKind    `json:"kind"`
	RouteID           string      `json:"route_id,omitempty"`
	RecordID          string      `json:"record_id,omitempty"`
	Record            *Record     `json:"record,omitempty"`
	ErrorRules        []ErrorRule `json:"error_rules,omitempty"`
	WidthMS           int64       `json:"width_ms,omitempty"`
	SlideMS           int64       `json:"slide_ms,omitempty"`
	AllowedLatenessMS int64       `json:"allowed_lateness_ms,omitempty"`
	WatermarkMS       *int64      `json:"watermark_ms,omitempty"`
}

type DependencyReport struct {
	EditKind          string   `json:"edit_kind"`
	Sources           []string `json:"sources"`
	InvalidWindowKeys []string `json:"invalid_window_keys"`
	RecomputedWindows []string `json:"recomputed_windows"`
	ReusedWindows     []string `json:"reused_windows"`
	RemovedWindows    []string `json:"removed_windows"`
	InvalidBatchNos   []int    `json:"invalid_batch_nos"`
	RecomputedBatches []int    `json:"recomputed_batches"`
	ReusedBatches     []int    `json:"reused_batches"`
	BatchClosure      []int    `json:"batch_closure"`
	PropagationEdges  []string `json:"propagation_edges"`
}

type IncrementalResult struct {
	Report     *RunReport       `json:"report"`
	Dependency DependencyReport `json:"dependency"`
}

type IncrementalEngine struct {
	project         *Project
	engine          *Engine
	cache           map[string]*Aggregate
	events          []RecomputeEvent
	drops           []DroppedRecord
	batches         []*ExportBatch
	recordWindows   map[string]map[string]bool
	windowRecords   map[string]map[string]bool
	routeWindows    map[string]map[string]bool
	latenessWindows map[string]bool
	windowBatches   map[string]int
}

func NewIncrementalEngine(source *Project) (*IncrementalEngine, error) {
	project := source.Clone()
	engine, err := NewEngine(project)
	if err != nil {
		return nil, err
	}
	report, err := engine.Run()
	if err != nil {
		return nil, err
	}
	ie := &IncrementalEngine{
		project: project, engine: engine, cache: map[string]*Aggregate{},
		events: report.RecomputeEvents, drops: report.Dropped, batches: report.Batches,
		recordWindows: map[string]map[string]bool{}, windowRecords: map[string]map[string]bool{},
		routeWindows: map[string]map[string]bool{}, latenessWindows: map[string]bool{},
		windowBatches: map[string]int{},
	}
	for _, agg := range report.Windows {
		ie.cache[agg.Key.String()] = agg
	}
	for _, event := range report.RecomputeEvents {
		ie.latenessWindows[event.WindowKey.String()] = true
	}
	for _, drop := range report.Dropped {
		ie.latenessWindows[drop.WindowKey.String()] = true
	}
	for _, batch := range report.Batches {
		for _, key := range batch.WindowKeys {
			ie.windowBatches[key] = batch.BatchNo
		}
	}
	ie.rebuildCandidateIndex()
	return ie, nil
}

func (ie *IncrementalEngine) Project() *Project { return ie.project.Clone() }

func (ie *IncrementalEngine) Snapshot() *RunReport {
	windows := ie.sortedWindows()
	return &RunReport{
		ProjectVersion: ie.project.ProjectVersion, Windows: windows,
		RecomputeEvents: append([]RecomputeEvent(nil), ie.events...),
		RecomputedKeys:  recomputedKeySet(ie.events),
		Dropped:         append([]DroppedRecord(nil), ie.drops...),
		Batches:         ie.batches,
	}
}

func (ie *IncrementalEngine) Apply(edit Edit) (*IncrementalResult, error) {
	before := ie.project.Clone()
	dep := DependencyReport{EditKind: string(edit.Kind)}
	invalid := map[string]bool{}

	switch edit.Kind {
	case EditWindowConfig:
		dep.Sources = []string{"config:window"}
		for key := range ie.cache {
			invalid[key] = true
		}
		ie.project.Window.WidthMS = edit.WidthMS
		ie.project.Window.SlideMS = edit.SlideMS
		if err := ie.resetEngine(); err != nil {
			ie.project = before
			_ = ie.resetEngine()
			return nil, err
		}
		full, err := ie.engine.Run()
		if err != nil {
			ie.project = before
			_ = ie.resetEngine()
			return nil, err
		}
		ie.cache = map[string]*Aggregate{}
		for _, agg := range full.Windows {
			ie.cache[agg.Key.String()] = agg
			invalid[agg.Key.String()] = true
		}
		ie.events = full.RecomputeEvents
		ie.drops = full.Dropped
		ie.batches = full.Batches
		ie.rebuildCandidateIndex()
		ie.rebuildBatchIndex()
		dep.InvalidWindowKeys = sortedKeys(invalid)
		dep.RecomputedWindows = sortedKeys(invalid)
		dep.ReusedWindows = []string{}
		dep.RecomputedBatches = batchNumbers(full.Batches)
		dep.BatchClosure = dep.RecomputedBatches
		dep.PropagationEdges = []string{"config:window -> all_windows -> all_batches"}
		return &IncrementalResult{Report: ie.Snapshot(), Dependency: dep}, nil

	case EditErrorRule:
		dep.Sources = []string{"route:" + edit.RouteID + ":error_rules"}
		ie.addRouteClosure(edit.RouteID, invalid)
		if err := ie.mutate(func() error {
			route := ie.project.RouteByID(edit.RouteID)
			if route == nil {
				return NewError(CodeFieldRequired, "edit.route_id", "route not found").WithContext("route_id", edit.RouteID)
			}
			route.ErrorRules = append([]ErrorRule(nil), edit.ErrorRules...)
			return nil
		}); err != nil {
			return nil, err
		}

	case EditLateness:
		dep.Sources = []string{"config:allowed_lateness"}
		for key := range ie.latenessWindows {
			invalid[key] = true
		}
		ie.addParents(invalid)
		if err := ie.mutate(func() error { ie.project.AllowedLatenessMS = edit.AllowedLatenessMS; return nil }); err != nil {
			return nil, err
		}

	case EditAddRecord:
		if edit.Record == nil {
			return nil, NewError(CodeFieldRequired, "edit.record", "record is required for add")
		}
		dep.Sources = []string{"record:" + edit.Record.ID}
		if err := ie.mutate(func() error {
			ie.project.Records = append(ie.project.Records, *edit.Record)
			return nil
		}); err != nil {
			return nil, err
		}
		ie.rebuildCandidateIndex()
		ie.addRecordClosure(edit.Record.ID, invalid)

	case EditDeleteRecord:
		dep.Sources = []string{"record:" + edit.RecordID}
		ie.addRecordClosure(edit.RecordID, invalid)
		if err := ie.mutate(func() error {
			next := ie.project.Records[:0]
			for _, record := range ie.project.Records {
				if record.ID != edit.RecordID {
					next = append(next, record)
				}
			}
			ie.project.Records = next
			return nil
		}); err != nil {
			return nil, err
		}
		delete(ie.recordWindows, edit.RecordID)

	case EditUpdateRecord:
		if edit.Record == nil {
			return nil, NewError(CodeFieldRequired, "edit.record", "record is required for update")
		}
		dep.Sources = []string{"record:" + edit.Record.ID}
		ie.addRecordClosure(edit.Record.ID, invalid)
		if err := ie.mutate(func() error {
			for i := range ie.project.Records {
				if ie.project.Records[i].ID == edit.Record.ID {
					ie.project.Records[i] = *edit.Record
					return nil
				}
			}
			ie.project.Records = append(ie.project.Records, *edit.Record)
			return nil
		}); err != nil {
			return nil, err
		}
		ie.rebuildCandidateIndex()
		ie.addRecordClosure(edit.Record.ID, invalid)

	case EditWatermark:
		dep.Sources = []string{"watermark:explicit"}
		for key := range ie.latenessWindows {
			invalid[key] = true
		}
		ie.addParents(invalid)
		if err := ie.mutate(func() error { ie.project.WatermarkMS = edit.WatermarkMS; return nil }); err != nil {
			return nil, err
		}

	default:
		return nil, NewError(CodeFieldType, "edit.kind", "unknown edit kind").WithContext("kind", edit.Kind)
	}

	if err := ie.resetEngine(); err != nil {
		return nil, err
	}
	ie.rebuildCandidateIndex()

	oldInvalidBatches := ie.oldBatchClosure(invalid)
	dep.InvalidWindowKeys = sortedKeys(invalid)
	dep.InvalidBatchNos = oldInvalidBatches

	if err := ie.recomputeInvalid(invalid); err != nil {
		return nil, err
	}
	ie.refreshCommitFlags()

	newBatches, err := BuildBatches(ie.project, ie.sortedWindows())
	if err != nil {
		return nil, err
	}
	recomputedBatches, reusedBatches, batchClosure := ie.classifyBatches(newBatches, oldInvalidBatches)
	ie.batches = newBatches
	ie.rebuildBatchIndex()
	ie.rebuildLatenessIndexFromEvents()

	reusedWindows := []string{}
	for key := range ie.cache {
		if !invalid[key] {
			reusedWindows = append(reusedWindows, key)
		}
	}
	sort.Strings(reusedWindows)
	removed := []string{}
	for _, key := range dep.InvalidWindowKeys {
		if ie.cache[key] == nil {
			removed = append(removed, key)
		}
	}

	dep.RecomputedWindows = filterPresent(invalid, ie.cache)
	dep.ReusedWindows = reusedWindows
	dep.RemovedWindows = removed
	dep.RecomputedBatches = recomputedBatches
	dep.ReusedBatches = reusedBatches
	dep.BatchClosure = batchClosure
	dep.PropagationEdges = ie.propagationEdges(dep.Sources, dep.InvalidWindowKeys, batchClosure)
	return &IncrementalResult{Report: ie.Snapshot(), Dependency: dep}, nil
}

func (ie *IncrementalEngine) mutate(fn func() error) error {
	backup := ie.project.Clone()
	if err := fn(); err != nil {
		ie.project = backup
		_ = ie.resetEngine()
		return err
	}
	if err := ie.project.Validate(); err != nil {
		ie.project = backup
		_ = ie.resetEngine()
		return err
	}
	return ie.resetEngine()
}

func (ie *IncrementalEngine) resetEngine() error {
	engine, err := NewEngine(ie.project)
	if err != nil {
		return err
	}
	ie.engine = engine
	return nil
}

func (ie *IncrementalEngine) rebuildCandidateIndex() {
	ie.recordWindows = map[string]map[string]bool{}
	ie.windowRecords = map[string]map[string]bool{}
	ie.routeWindows = map[string]map[string]bool{}
	for _, record := range ie.project.Records {
		keys, err := groupKeysForRecord(ie.project, record)
		if err != nil {
			continue
		}
		for _, key := range keys {
			text := key.String()
			addSet(ie.recordWindows, record.ID, text)
			addSet(ie.windowRecords, text, record.ID)
			if record.RouteID != "" {
				addSet(ie.routeWindows, record.RouteID, text)
			}
		}
	}
}

func (ie *IncrementalEngine) rebuildBatchIndex() {
	ie.windowBatches = map[string]int{}
	for _, batch := range ie.batches {
		for _, key := range batch.WindowKeys {
			ie.windowBatches[key] = batch.BatchNo
		}
	}
}

func (ie *IncrementalEngine) rebuildLatenessIndexFromEvents() {
	ie.latenessWindows = map[string]bool{}
	for _, event := range ie.events {
		ie.latenessWindows[event.WindowKey.String()] = true
	}
	for _, drop := range ie.drops {
		ie.latenessWindows[drop.WindowKey.String()] = true
	}
}

func addSet(mapOfSets map[string]map[string]bool, id, value string) {
	set := mapOfSets[id]
	if set == nil {
		set = map[string]bool{}
		mapOfSets[id] = set
	}
	set[value] = true
}

func groupKeysForRecord(p *Project, record Record) ([]WindowKey, error) {
	if record.ServiceID == "" || record.RouteID == "" {
		route, err := p.MatchRoute(record.Path)
		if err != nil {
			return nil, err
		}
		record.RouteID = route.ID
		record.ServiceID = route.ServiceID
	}
	indices, err := p.WindowIndices(record.EventTimeMS)
	if err != nil {
		return nil, err
	}
	engine := &Engine{project: p}
	keys := make([]WindowKey, 0, len(indices)*3)
	for _, index := range indices {
		keys = append(keys, engine.groupKeys(index, record)...)
	}
	return keys, nil
}

func (ie *IncrementalEngine) addRecordClosure(recordID string, invalid map[string]bool) {
	for key := range ie.recordWindows[recordID] {
		invalid[key] = true
	}
	ie.addParents(invalid)
}

func (ie *IncrementalEngine) addRouteClosure(routeID string, invalid map[string]bool) {
	for key := range ie.routeWindows[routeID] {
		invalid[key] = true
	}
	ie.addParents(invalid)
}

func (ie *IncrementalEngine) addParents(invalid map[string]bool) {
	original := sortedKeys(invalid)
	for _, text := range original {
		key, err := ParseWindowKey(text)
		if err != nil {
			continue
		}
		if key.Level == "status" {
			parent := WindowKey{Index: key.Index, GroupKey: GroupKey{Level: "route", ServiceID: key.ServiceID, RouteID: key.RouteID}}
			invalid[parent.String()] = true
			service := WindowKey{Index: key.Index, GroupKey: GroupKey{Level: "service", ServiceID: key.ServiceID}}
			invalid[service.String()] = true
		}
		if key.Level == "route" {
			service := WindowKey{Index: key.Index, GroupKey: GroupKey{Level: "service", ServiceID: key.ServiceID}}
			invalid[service.String()] = true
		}
	}
}

func (ie *IncrementalEngine) recomputeInvalid(invalid map[string]bool) error {
	recordIDs := map[string]bool{}
	for keyText := range invalid {
		for id := range ie.windowRecords[keyText] {
			recordIDs[id] = true
		}
	}
	records := make([]Record, 0, len(recordIDs))
	recordByID := map[string]Record{}
	for _, record := range ie.project.Records {
		recordByID[record.ID] = record
	}
	for id := range recordIDs {
		record, ok := recordByID[id]
		if !ok {
			continue
		}
		records = append(records, record)
	}

	var part *partitionResult
	if len(records) > 0 {
		var err error
		part, err = ie.engine.processRecords(records, "incremental")
		if err != nil {
			return err
		}
	}

	for keyText := range invalid {
		if part != nil && part.aggs[keyText] != nil {
			agg := part.aggs[keyText]
			sort.Strings(agg.RecordIDs)
			ie.cache[keyText] = agg
		} else {
			delete(ie.cache, keyText)
		}
	}

	ie.events = filterEventsOutside(ie.events, invalid)
	ie.drops = filterDropsOutside(ie.drops, invalid)
	if part != nil {
		for _, event := range part.recompute {
			if invalid[event.WindowKey.String()] {
				ie.events = append(ie.events, event)
			}
		}
		for _, drop := range part.dropped {
			if invalid[drop.WindowKey.String()] {
				ie.drops = append(ie.drops, drop)
			}
		}
	}
	ie.events = dedupeRecomputeEvents(ie.events)
	ie.drops = dedupeDrops(ie.drops)
	return nil
}

func (ie *IncrementalEngine) refreshCommitFlags() {
	wm := ie.project.WatermarkMS
	if len(ie.project.WatermarkMarks) > 0 {
		maxMark := ie.project.WatermarkMarks[0].WatermarkMS
		for _, mark := range ie.project.WatermarkMarks {
			if mark.WatermarkMS > maxMark {
				maxMark = mark.WatermarkMS
			}
		}
		if wm == nil || maxMark > *wm {
			wm = &maxMark
		}
	}
	if wm == nil && ie.project.CommittedBoundaryMS != nil {
		wm = ie.project.CommittedBoundaryMS
	}
	for _, agg := range ie.cache {
		agg.Committed = wm != nil && *wm >= agg.EndMS+ie.project.AllowedLatenessMS
	}
}

func (ie *IncrementalEngine) sortedWindows() []*Aggregate {
	out := make([]*Aggregate, 0, len(ie.cache))
	for _, agg := range ie.cache {
		out = append(out, agg)
	}
	sort.Slice(out, func(i, j int) bool { return CompareWindowKey(out[i].Key, out[j].Key) < 0 })
	return out
}

func (ie *IncrementalEngine) oldBatchClosure(invalid map[string]bool) []int {
	minNo := 0
	for key := range invalid {
		if no, ok := ie.windowBatches[key]; ok && (minNo == 0 || no < minNo) {
			minNo = no
		}
	}
	if minNo == 0 {
		return []int{}
	}
	out := []int{}
	for _, batch := range ie.batches {
		if batch.BatchNo >= minNo {
			out = append(out, batch.BatchNo)
		}
	}
	return out
}

func (ie *IncrementalEngine) classifyBatches(newBatches []*ExportBatch, oldClosure []int) (recomputed, reused, closure []int) {
	oldByNo := map[int]*ExportBatch{}
	for _, batch := range ie.batches {
		oldByNo[batch.BatchNo] = batch
	}
	firstChanged := 0
	for _, batch := range newBatches {
		old := oldByNo[batch.BatchNo]
		if old == nil || BatchIdentity(old) != BatchIdentity(batch) {
			if firstChanged == 0 || batch.BatchNo < firstChanged {
				firstChanged = batch.BatchNo
			}
		}
	}
	for _, no := range oldClosure {
		if firstChanged == 0 || no < firstChanged {
			firstChanged = no
		}
	}
	for _, batch := range newBatches {
		if firstChanged != 0 && batch.BatchNo >= firstChanged {
			recomputed = append(recomputed, batch.BatchNo)
			closure = append(closure, batch.BatchNo)
		} else {
			reused = append(reused, batch.BatchNo)
		}
	}
	return recomputed, reused, closure
}

func (ie *IncrementalEngine) propagationEdges(sources, keys []string, batches []int) []string {
	edges := []string{}
	for _, source := range sources {
		for i, key := range keys {
			if i >= 12 {
				edges = append(edges, source+" -> "+key+" -> ...")
				break
			}
			if batchNo, ok := ie.windowBatches[key]; ok {
				edges = append(edges, source+" -> window:"+key+" -> batch:"+itoa(batchNo))
			} else {
				edges = append(edges, source+" -> window:"+key)
			}
		}
	}
	return edges
}

func filterEventsOutside(events []RecomputeEvent, invalid map[string]bool) []RecomputeEvent {
	out := events[:0]
	for _, event := range events {
		if !invalid[event.WindowKey.String()] {
			out = append(out, event)
		}
	}
	return out
}

func filterDropsOutside(drops []DroppedRecord, invalid map[string]bool) []DroppedRecord {
	out := drops[:0]
	for _, drop := range drops {
		if !invalid[drop.WindowKey.String()] {
			out = append(out, drop)
		}
	}
	return out
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func filterPresent(set map[string]bool, cache map[string]*Aggregate) []string {
	out := []string{}
	for key := range set {
		if cache[key] != nil {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

func batchNumbers(batches []*ExportBatch) []int {
	out := make([]int, 0, len(batches))
	for _, batch := range batches {
		out = append(out, batch.BatchNo)
	}
	sort.Ints(out)
	return out
}

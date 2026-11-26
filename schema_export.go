package mirror

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
)

// Export serializes the internal session to one of the declared schema
// versions. If a value cannot be represented by the target version, no
// document is returned and the error contains the complete loss list.
func Export(s *Session, version string) ([]byte, []Loss, error) {
	if s == nil {
		return nil, nil, errf(CodeSchema, "session", "session is nil")
	}
	normalized, ok := normalizeVersionPin(version)
	if !ok {
		return nil, nil, errf(CodeSchema, "schema_version", "unknown export version %q", version)
	}
	cp := cloneSession(s)
	cp.SourceVersion = normalized
	if err := NormalizeSession(cp); err != nil {
		return nil, nil, err
	}
	var (
		doc    any
		losses []Loss
		err    error
	)
	switch normalized {
	case SchemaV1:
		doc, losses, err = exportV1(cp)
	case SchemaV2:
		doc, losses, err = exportV2(cp)
	case SchemaV3:
		doc, losses, err = exportV3(cp)
	}
	if err != nil {
		return nil, losses, err
	}
	if len(losses) > 0 {
		e := &Error{Code: CodeLossy, Path: losses[0].Path,
			Message: "internal model contains values that the target schema cannot represent",
			Losses:  losses}
		return nil, losses, e
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, losses, errf(CodeSchema, "$", "encode JSON: %v", err)
	}
	data = append(data, '\n')
	return data, nil, nil
}

// RoundTripSession exports and reloads a session. A mismatch is reported as
// E_LOSSY with the first non-recoverable field path.
func RoundTripSession(s *Session, version string) (*Session, []byte, []Loss, error) {
	data, losses, err := Export(s, version)
	if err != nil {
		return nil, nil, losses, err
	}
	loaded, err := LoadVersion(data, version)
	if err != nil {
		return nil, data, losses, err
	}
	if path, a, b, diff := FirstCoreDifference(s, loaded); diff {
		loss := Loss{Path: path, Value: fmt.Sprintf("%v != %v", a, b),
			Reason: "round-trip cannot restore equivalent internal value", Disposition: "reject export"}
		return nil, data, []Loss{loss}, &Error{Code: CodeLossy, Path: path,
			Message: "round-trip mismatch after export", Losses: []Loss{loss}}
	}
	return loaded, data, nil, nil
}

func cloneSession(s *Session) *Session {
	cp := *s
	cp.Domains = append([]Domain(nil), s.Domains...)
	cp.Screens = append([]Screen(nil), s.Screens...)
	for i := range cp.Screens {
		cp.Screens[i].ScaleCapabilities = append([]Scale(nil), s.Screens[i].ScaleCapabilities...)
		cp.Screens[i].ChromaCapabilities = append([]Chroma(nil), s.Screens[i].ChromaCapabilities...)
	}
	cp.Events = append([]Event(nil), s.Events...)
	cp.Windows = append([]Window(nil), s.Windows...)
	cp.Traces = append([]ConversionTrace(nil), s.Traces...)
	cp.Warnings = append([]Loss(nil), s.Warnings...)
	return &cp
}

func exportV1(s *Session) (any, []Loss, error) {
	losses := []Loss{}
	domains := make([]any, 0, len(s.Domains))
	for i, d := range s.Domains {
		domains = append(domains, map[string]any{
			"id": d.ID, "parent": d.Parent,
			"origin": map[string]any{"x": d.OriginX, "y": d.OriginY},
		})
		_ = i
	}
	screens := make([]any, 0, len(s.Screens))
	for i, sc := range s.Screens {
		path := fmt.Sprintf("screens[%d]", i)
		scaleCodes := make([]any, 0, len(sc.ScaleCapabilities))
		for j, v := range sc.ScaleCapabilities {
			code, loss := scaleToV1(v, fmt.Sprintf("%s.scale_profiles[%d]", path, j))
			if loss != nil {
				losses = append(losses, *loss)
				continue
			}
			scaleCodes = append(scaleCodes, code)
		}
		chromaCodes := make([]any, 0, len(sc.ChromaCapabilities))
		for j, v := range sc.ChromaCapabilities {
			code, loss := chromaToV1(v, fmt.Sprintf("%s.chroma_modes[%d]", path, j))
			if loss != nil {
				losses = append(losses, *loss)
				continue
			}
			chromaCodes = append(chromaCodes, code)
		}
		defaultScale, loss := scaleToV1(sc.DefaultScale, path+".default_scale")
		if loss != nil {
			losses = append(losses, *loss)
		}
		defaultChroma, loss := chromaToV1(sc.DefaultChroma, path+".default_chroma")
		if loss != nil {
			losses = append(losses, *loss)
		}
		item := map[string]any{
			"id": sc.ID, "name": sc.Name, "domain": sc.Domain, "master": sc.Master,
			"origin":           map[string]any{"x": sc.OriginX, "y": sc.OriginY},
			"resolution":       map[string]any{"width": sc.Width, "height": sc.Height},
			"scale_profiles":   scaleCodes,
			"chroma_modes":     chromaCodes,
			"max_packet_bytes": sc.MaxPacketBytes,
		}
		// Omitted defaults are intentionally used to exercise the documented
		// v1 default-fill upgrade path.
		if sc.DefaultScale != Scale100 {
			item["default_scale"] = defaultScale
		}
		if sc.DefaultChroma != Chroma444 {
			item["default_chroma"] = defaultChroma
		}
		screens = append(screens, item)
	}
	events := make([]any, 0, len(s.Events))
	for _, ev := range s.Events {
		events = append(events, map[string]any{
			"id": ev.ID, "kind": string(ev.Kind), "screen_id": ev.ScreenID,
			"domain": ev.Domain, "from_domain": ev.FromDomain, "to_domain": ev.ToDomain,
			"at_tick": ev.AtTick, "timeout_ticks": ev.TimeoutTicks,
		})
	}
	windows := make([]any, 0, len(s.Windows))
	for i, w := range s.Windows {
		path := fmt.Sprintf("windows[%d]", i)
		scaleCode, loss := scaleToV1(w.Scale, path+".scale")
		if loss != nil {
			losses = append(losses, *loss)
		}
		chromaCode, loss := chromaToV1(w.Chroma, path+".chroma")
		if loss != nil {
			losses = append(losses, *loss)
		}
		windows = append(windows, map[string]any{
			"id": w.ID, "index": w.Index, "start_tick": w.StartTick, "end_tick": w.EndTick,
			"region": map[string]any{"x": w.Region.X, "y": w.Region.Y, "width": w.Region.W, "height": w.Region.H},
			"scale":  scaleCode, "chroma": chromaCode, "max_packet_bytes": w.MaxPacketBytes,
		})
	}
	doc := map[string]any{
		"schema_version": "1.0",
		"session_id":     s.ID, "seed": s.Seed, "algorithm_version": s.AlgorithmVersion,
		"tick_ms":                   s.TickMillis,
		"negotiation_timeout_ticks": s.NegotiationTimeoutTicks,
		"reconnect_timeout_ticks":   s.ReconnectTimeoutTicks,
		"master_screen_id":          s.MasterScreenID,
		"rounding":                  map[string]any{"time": "floor", "coord": "floor"},
		"defaults":                  map[string]any{"scale": "100%", "chroma": "444", "max_packet_bytes": int64(65536)},
		"domains":                   domains, "screens": screens, "events": events, "windows": windows,
	}
	if len(losses) > 0 {
		return nil, losses, &Error{Code: CodeLossy, Path: losses[0].Path,
			Message: "schema 1 cannot represent all internal values", Losses: losses}
	}
	return doc, nil, nil
}

func exportV2(s *Session) (any, []Loss, error) {
	losses := []Loss{}
	master, ok := s.ScreenByID(s.MasterScreenID)
	if !ok {
		return nil, nil, errf(CodeSchema, "master_screen_id", "unknown screen")
	}
	domainGroups := make([]any, 0, len(s.Domains))
	for _, d := range s.Domains {
		groupScreens := []any{}
		for i, sc := range s.Screens {
			if sc.Domain != d.ID {
				continue
			}
			path := fmt.Sprintf("topology[%s].screens[%d]", d.ID, i)
			scaleCodes := []any{}
			for j, v := range sc.ScaleCapabilities {
				code, loss := scaleToV2(v, fmt.Sprintf("%s.scaleProfiles[%d]", path, j))
				if loss != nil {
					losses = append(losses, *loss)
					continue
				}
				scaleCodes = append(scaleCodes, code)
			}
			chromaCodes := []any{}
			for j, v := range sc.ChromaCapabilities {
				code, loss := chromaToV2(v, fmt.Sprintf("%s.chromaModes[%d]", path, j))
				if loss != nil {
					losses = append(losses, *loss)
					continue
				}
				chromaCodes = append(chromaCodes, code)
			}
			defaultScale, loss := scaleToV2(sc.DefaultScale, path+".defaultScale")
			if loss != nil {
				losses = append(losses, *loss)
			}
			defaultChroma, loss := chromaToV2(sc.DefaultChroma, path+".defaultChroma")
			if loss != nil {
				losses = append(losses, *loss)
			}
			groupScreens = append(groupScreens, map[string]any{
				"screenId": sc.ID, "displayName": sc.Name, "master": sc.Master,
				"originX": sc.OriginX, "originY": sc.OriginY,
				"resolution":    map[string]any{"w": sc.Width, "h": sc.Height},
				"scaleProfiles": scaleCodes, "defaultScale": defaultScale,
				"chromaModes": chromaCodes, "defaultChroma": defaultChroma,
				"maxPacketBytes": sc.MaxPacketBytes,
			})
		}
		domainGroups = append(domainGroups, map[string]any{
			"domain": d.ID, "parent": d.Parent,
			"origin":  map[string]any{"x": d.OriginX, "y": d.OriginY},
			"screens": groupScreens,
		})
	}
	events := make([]any, 0, len(s.Events))
	for _, ev := range s.Events {
		atMS, err := ticksToMillis("events.atMillis", ev.AtTick, s.TickMillis)
		if err != nil {
			return nil, losses, err
		}
		timeoutMS := int64(0)
		if ev.TimeoutTicks != 0 {
			timeoutMS, err = ticksToMillis("events.timeoutMillis", ev.TimeoutTicks, s.TickMillis)
			if err != nil {
				return nil, losses, err
			}
		}
		events = append(events, map[string]any{
			"id": ev.ID, "type": string(ev.Kind), "screenId": ev.ScreenID,
			"domain": ev.Domain, "fromDomain": ev.FromDomain, "toDomain": ev.ToDomain,
			"atMillis": atMS, "timeoutMillis": timeoutMS,
		})
	}
	windows := make([]any, 0, len(s.Windows))
	for i, w := range s.Windows {
		path := fmt.Sprintf("windows[%d]", i)
		scaleCode, loss := scaleToV2(w.Scale, path+".scale")
		if loss != nil {
			losses = append(losses, *loss)
		}
		chromaCode, loss := chromaToV2(w.Chroma, path+".chroma")
		if loss != nil {
			losses = append(losses, *loss)
		}
		dx, err := SubChecked(path+".region.dx", w.Region.X, master.OriginX)
		if err != nil {
			return nil, losses, err
		}
		dy, err := SubChecked(path+".region.dy", w.Region.Y, master.OriginY)
		if err != nil {
			return nil, losses, err
		}
		if dx < minOffset32 || dx > maxOffset32 || dy < minOffset32 || dy > maxOffset32 {
			losses = append(losses, Loss{Path: path + ".region", Value: fmt.Sprintf("%d,%d", dx, dy),
				Reason: "schema 2 offsets are signed 32-bit", Disposition: "reject export"})
		}
		startMS, err := ticksToMillis(path+".startMillis", w.StartTick, s.TickMillis)
		if err != nil {
			return nil, losses, err
		}
		endMS, err := ticksToMillis(path+".endMillis", w.EndTick, s.TickMillis)
		if err != nil {
			return nil, losses, err
		}
		windows = append(windows, map[string]any{
			"id": w.ID, "sequence": w.Index,
			"startMillis": startMS, "endMillis": endMS,
			"scale": scaleCode, "chroma": chromaCode, "maxPacketBytes": w.MaxPacketBytes,
			"region": map[string]any{
				"originScreenId": s.MasterScreenID,
				"dx":             dx, "dy": dy, "dw": w.Region.W, "dh": w.Region.H,
			},
		})
	}
	negMS, err := ticksToMillis("timebase.negotiationTimeoutMillis", s.NegotiationTimeoutTicks, s.TickMillis)
	if err != nil {
		return nil, losses, err
	}
	recMS, err := ticksToMillis("timebase.reconnectTimeoutMillis", s.ReconnectTimeoutTicks, s.TickMillis)
	if err != nil {
		return nil, losses, err
	}
	doc := map[string]any{
		"schemaVersion": 2,
		"session": map[string]any{
			"id": s.ID, "seed": s.Seed, "algorithmVersion": s.AlgorithmVersion,
			"masterScreenId": s.MasterScreenID,
		},
		"timebase": map[string]any{
			"tickMillis":               s.TickMillis,
			"negotiationTimeoutMillis": negMS,
			"reconnectTimeoutMillis":   recMS,
		},
		"rounding":       map[string]any{"time": "floor", "coord": "floor"},
		"coordinateMode": "screen-origin-offset",
		"topology":       domainGroups, "events": events, "windows": windows,
	}
	if len(losses) > 0 {
		return nil, losses, &Error{Code: CodeLossy, Path: losses[0].Path,
			Message: "schema 2 cannot represent all internal values", Losses: losses}
	}
	return doc, nil, nil
}

type v3DomainNode struct {
	Domain
	Screens  []Screen
	Children []*v3DomainNode
}

func exportV3(s *Session) (any, []Loss, error) {
	losses := []Loss{}
	master, ok := s.ScreenByID(s.MasterScreenID)
	if !ok {
		return nil, nil, errf(CodeSchema, "master_screen_id", "unknown screen")
	}
	nodes := map[string]*v3DomainNode{}
	for _, d := range s.Domains {
		nodes[d.ID] = &v3DomainNode{Domain: d}
	}
	for _, sc := range s.Screens {
		if n := nodes[sc.Domain]; n != nil {
			n.Screens = append(n.Screens, sc)
		}
	}
	roots := []*v3DomainNode{}
	domainIDs := make([]string, 0, len(s.Domains))
	for id := range nodes {
		domainIDs = append(domainIDs, id)
	}
	sort.Strings(domainIDs)
	for _, id := range domainIDs {
		n := nodes[id]
		if n.Parent == "" {
			roots = append(roots, n)
			continue
		}
		parent := nodes[n.Parent]
		if parent == nil {
			return nil, nil, errf(CodeSchema, "domains."+n.ID+".parent", "unknown parent domain %q", n.Parent)
		}
		parent.Children = append(parent.Children, n)
	}
	rootDocs := make([]any, 0, len(roots))
	for _, root := range roots {
		rootDocs = append(rootDocs, exportV3Domain(root, &losses))
	}
	events := make([]any, 0, len(s.Events))
	for _, ev := range s.Events {
		atMS, err := ticksToMillis("events.at", ev.AtTick, s.TickMillis)
		if err != nil {
			return nil, losses, err
		}
		item := map[string]any{
			"id": ev.ID, "kind": upperEventKind(ev.Kind), "screenId": ev.ScreenID,
			"domain": ev.Domain, "fromDomain": ev.FromDomain, "toDomain": ev.ToDomain,
			"at": millisToTimeObject(atMS),
		}
		if ev.TimeoutTicks != 0 {
			toMS, err := ticksToMillis("events.timeout", ev.TimeoutTicks, s.TickMillis)
			if err != nil {
				return nil, losses, err
			}
			item["timeout"] = millisToTimeObject(toMS)
		}
		events = append(events, item)
	}
	windows := make([]any, 0, len(s.Windows))
	for i, w := range s.Windows {
		path := fmt.Sprintf("windows[%d]", i)
		startMS, err := ticksToMillis(path+".time.start", w.StartTick, s.TickMillis)
		if err != nil {
			return nil, losses, err
		}
		endMS, err := ticksToMillis(path+".time.end", w.EndTick, s.TickMillis)
		if err != nil {
			return nil, losses, err
		}
		scaleCode, loss := scaleToV3(w.Scale)
		if loss != nil {
			loss.Path = path + ".scale"
			losses = append(losses, *loss)
		}
		chromaCode, loss := chromaToV3(w.Chroma)
		if loss != nil {
			loss.Path = path + ".chroma"
			losses = append(losses, *loss)
		}
		dx, err := SubChecked(path+".region.dx", w.Region.X, master.OriginX)
		if err != nil {
			return nil, losses, err
		}
		dy, err := SubChecked(path+".region.dy", w.Region.Y, master.OriginY)
		if err != nil {
			return nil, losses, err
		}
		if dx < minOffset32 || dx > maxOffset32 || dy < minOffset32 || dy > maxOffset32 {
			losses = append(losses, Loss{Path: path + ".region", Value: fmt.Sprintf("%d,%d", dx, dy),
				Reason: "schema 3 offsets are signed 32-bit", Disposition: "reject export"})
		}
		windows = append(windows, map[string]any{
			"id": w.ID, "index": w.Index,
			"time": map[string]any{
				"start": millisToTimeObject(startMS),
				"end":   millisToTimeObject(endMS),
			},
			"scale": scaleCode, "chroma": chromaCode, "maxPacketBytes": w.MaxPacketBytes,
			"region": map[string]any{
				"originScreenId": s.MasterScreenID, "dx": dx, "dy": dy,
				"w": w.Region.W, "h": w.Region.H,
			},
		})
	}
	negMS, err := ticksToMillis("tick.negotiationTimeout", s.NegotiationTimeoutTicks, s.TickMillis)
	if err != nil {
		return nil, losses, err
	}
	recMS, err := ticksToMillis("tick.reconnectTimeout", s.ReconnectTimeoutTicks, s.TickMillis)
	if err != nil {
		return nil, losses, err
	}
	doc := map[string]any{
		"schemaVersion": "v3",
		"meta": map[string]any{
			"sessionId": s.ID, "seed": s.Seed, "algorithmVersion": s.AlgorithmVersion,
			"masterScreenId": s.MasterScreenID,
		},
		"tick": map[string]any{
			"durationMs":         s.TickMillis,
			"negotiationTimeout": millisToTimeObject(negMS),
			"reconnectTimeout":   millisToTimeObject(recMS),
		},
		"rounding":       map[string]any{"time": "floor", "coord": "floor"},
		"coordinateMode": "screen-origin-offset-tree",
		"topology":       map[string]any{"roots": rootDocs},
		"events":         events, "windows": windows,
	}
	if len(losses) > 0 {
		return nil, losses, &Error{Code: CodeLossy, Path: losses[0].Path,
			Message: "schema 3 cannot represent all internal values", Losses: losses}
	}
	return doc, nil, nil
}

func exportV3Domain(n *v3DomainNode, losses *[]Loss) map[string]any {
	screens := make([]any, 0, len(n.Screens))
	for i, sc := range n.Screens {
		path := fmt.Sprintf("topology.%s.screens[%d]", n.ID, i)
		scaleCodes := []any{}
		for j, v := range sc.ScaleCapabilities {
			code, loss := scaleToV3(v)
			if loss != nil {
				loss.Path = fmt.Sprintf("%s.capabilities.scaleProfiles[%d]", path, j)
				*losses = append(*losses, *loss)
				continue
			}
			scaleCodes = append(scaleCodes, code)
		}
		chromaCodes := []any{}
		for j, v := range sc.ChromaCapabilities {
			code, loss := chromaToV3(v)
			if loss != nil {
				loss.Path = fmt.Sprintf("%s.capabilities.chromaModes[%d]", path, j)
				*losses = append(*losses, *loss)
				continue
			}
			chromaCodes = append(chromaCodes, code)
		}
		defScale, loss := scaleToV3(sc.DefaultScale)
		if loss != nil {
			loss.Path = path + ".capabilities.defaultScale"
			*losses = append(*losses, *loss)
		}
		defChroma, loss := chromaToV3(sc.DefaultChroma)
		if loss != nil {
			loss.Path = path + ".capabilities.defaultChroma"
			*losses = append(*losses, *loss)
		}
		screens = append(screens, map[string]any{
			"id": sc.ID, "name": sc.Name, "master": sc.Master,
			"origin":     map[string]any{"x": sc.OriginX, "y": sc.OriginY},
			"resolution": map[string]any{"width": sc.Width, "height": sc.Height},
			"capabilities": map[string]any{
				"scaleProfiles": scaleCodes, "defaultScale": defScale,
				"chromaModes": chromaCodes, "defaultChroma": defChroma,
			},
			"maxPacketBytes": sc.MaxPacketBytes,
		})
	}
	children := make([]any, 0, len(n.Children))
	for _, child := range n.Children {
		children = append(children, exportV3Domain(child, losses))
	}
	doc := map[string]any{
		"id": n.ID, "origin": map[string]any{"x": n.OriginX, "y": n.OriginY},
		"screens": screens, "children": children,
	}
	return doc
}

func upperEventKind(k EventKind) string {
	switch k {
	case EventJoin:
		return "JOIN"
	case EventLeave:
		return "LEAVE"
	case EventTopology:
		return "TOPOLOGY"
	case EventNegotiate:
		return "NEGOTIATE"
	default:
		return string(k)
	}
}

func millisToTimeObject(ms int64) map[string]any {
	sec, rem := divMod(ms, 1000)
	return map[string]any{"sec": sec, "ms": rem}
}

// FirstCoreDifference finds the first semantic difference, excluding trace and
// source-version metadata. The path uses stable field names and slice indexes.
func FirstCoreDifference(a, b *Session) (path string, av, bv any, different bool) {
	ca := coreView(cloneSession(a))
	cb := coreView(cloneSession(b))
	return reflectDifference(reflect.ValueOf(ca), reflect.ValueOf(cb), "")
}

func reflectDifferenceAny(a, b any, path string) (string, any, any, bool) {
	return reflectDifference(reflect.ValueOf(a), reflect.ValueOf(b), path)
}

type sessionCore struct {
	ID                      string   `json:"id"`
	Seed                    int64    `json:"seed"`
	AlgorithmVersion        string   `json:"algorithmVersion"`
	TickMillis              int64    `json:"tickMillis"`
	NegotiationTimeoutTicks int64    `json:"negotiationTimeoutTicks"`
	ReconnectTimeoutTicks   int64    `json:"reconnectTimeoutTicks"`
	MasterScreenID          string   `json:"masterScreenId"`
	Domains                 []Domain `json:"domains"`
	Screens                 []Screen `json:"screens"`
	Events                  []Event  `json:"events"`
	Windows                 []Window `json:"windows"`
}

func coreView(s *Session) sessionCore {
	_ = NormalizeSession(s)
	for i := range s.Windows {
		s.Windows[i].SourceTimeResidualMs = 0
	}
	return sessionCore{
		ID: s.ID, Seed: s.Seed, AlgorithmVersion: s.AlgorithmVersion,
		TickMillis: s.TickMillis, NegotiationTimeoutTicks: s.NegotiationTimeoutTicks,
		ReconnectTimeoutTicks: s.ReconnectTimeoutTicks, MasterScreenID: s.MasterScreenID,
		Domains: s.Domains, Screens: s.Screens, Events: s.Events, Windows: s.Windows,
	}
}

func reflectDifference(a, b reflect.Value, path string) (string, any, any, bool) {
	if !a.IsValid() || !b.IsValid() {
		if a.IsValid() != b.IsValid() {
			return path, valueOf(a), valueOf(b), true
		}
		return "", nil, nil, false
	}
	if a.Type() != b.Type() {
		return path, valueOf(a), valueOf(b), true
	}
	switch a.Kind() {
	case reflect.Struct:
		for i := 0; i < a.NumField(); i++ {
			field := a.Type().Field(i)
			childPath := field.Name
			if path != "" {
				childPath = path + "." + field.Name
			}
			if p, av, bv, ok := reflectDifference(a.Field(i), b.Field(i), childPath); ok {
				return p, av, bv, true
			}
		}
	case reflect.Slice, reflect.Array:
		if a.Len() != b.Len() {
			return path + ".len", a.Len(), b.Len(), true
		}
		for i := 0; i < a.Len(); i++ {
			childPath := fmt.Sprintf("%s[%d]", path, i)
			if p, av, bv, ok := reflectDifference(a.Index(i), b.Index(i), childPath); ok {
				return p, av, bv, true
			}
		}
	case reflect.Map:
		keys := a.MapKeys()
		akeys := make([]string, 0, len(keys))
		for _, k := range keys {
			akeys = append(akeys, fmt.Sprint(k.Interface()))
		}
		sort.Strings(akeys)
		if a.Len() != b.Len() {
			return path + ".len", a.Len(), b.Len(), true
		}
		for _, k := range akeys {
			if p, av, bv, ok := reflectDifference(a.MapIndex(reflect.ValueOf(k)), b.MapIndex(reflect.ValueOf(k)), path+"."+k); ok {
				return p, av, bv, true
			}
		}
	case reflect.Interface, reflect.Pointer:
		if a.IsNil() != b.IsNil() {
			return path, valueOf(a), valueOf(b), true
		}
		if !a.IsNil() {
			return reflectDifference(a.Elem(), b.Elem(), path)
		}
	default:
		if !reflect.DeepEqual(a.Interface(), b.Interface()) {
			return path, a.Interface(), b.Interface(), true
		}
	}
	return "", nil, nil, false
}

func valueOf(v reflect.Value) any {
	if !v.IsValid() {
		return nil
	}
	return v.Interface()
}

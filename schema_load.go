package mirror

import (
	"fmt"
	"sort"
)

// Load parses one of the three supported schema documents and normalizes it
// into the internal model. Every conversion is recorded in Session.Traces.
func Load(data []byte) (*Session, error) {
	root, err := decodeJSONObject(data)
	if err != nil {
		return nil, err
	}
	version, err := detectSchemaVersion(root)
	if err != nil {
		return nil, err
	}
	var s *Session
	switch version {
	case SchemaV1:
		s, err = loadV1(root)
	case SchemaV2:
		s, err = loadV2(root)
	case SchemaV3:
		s, err = loadV3(root)
	default:
		err = errf(CodeSchema, "schema_version", "unknown schema version")
	}
	if err != nil {
		return nil, err
	}
	if err := NormalizeSession(s); err != nil {
		return nil, err
	}
	return s, nil
}

// LoadVersion loads a document and verifies that its declared source version
// is the requested version. It is useful for round-trip tests.
func LoadVersion(data []byte, want string) (*Session, error) {
	normalized, ok := normalizeVersionPin(want)
	if !ok {
		return nil, errf(CodeSchema, "schema_version", "unknown requested version %q", want)
	}
	s, err := Load(data)
	if err != nil {
		return nil, err
	}
	if s.SourceVersion != normalized {
		return nil, errf(CodeSchema, "schema_version", "loaded version %s, wanted %s", s.SourceVersion, normalized)
	}
	return s, nil
}

func normalizeVersionPin(v string) (string, bool) {
	switch v {
	case "1", "1.0":
		return SchemaV1, true
	case "2", "2.0":
		return SchemaV2, true
	case "3", "3.0", "v3":
		return SchemaV3, true
	default:
		return "", false
	}
}

func loadV1(root jsonObj) (*Session, error) {
	s := &Session{SourceVersion: SchemaV1}
	var err error
	if s.ID, err = requiredString(root, "session_id", "$"); err != nil {
		return nil, err
	}
	if s.AlgorithmVersion, err = optionalString(root, "algorithm_version", "$", AlgorithmVersion); err != nil {
		return nil, err
	}
	if v, ok := root.optional("seed"); ok {
		s.Seed, err = asInt64(v, "seed")
		if err != nil {
			return nil, err
		}
	}
	if s.TickMillis, err = requiredInt(root, "tick_ms", "$"); err != nil {
		return nil, err
	}
	if s.TickMillis <= 0 {
		return nil, errf(CodeSchema, "tick_ms", "must be greater than zero")
	}
	if s.NegotiationTimeoutTicks, err = requiredInt(root, "negotiation_timeout_ticks", "$"); err != nil {
		return nil, err
	}
	if s.ReconnectTimeoutTicks, err = requiredInt(root, "reconnect_timeout_ticks", "$"); err != nil {
		return nil, err
	}
	if s.MasterScreenID, err = requiredString(root, "master_screen_id", "$"); err != nil {
		return nil, err
	}
	rounding, err := parseRounding(root)
	if err != nil {
		return nil, err
	}
	s.TimeRounding = rounding
	defaults, err := requiredObject(root, "defaults", "$")
	if err != nil {
		return nil, err
	}
	defaultScaleRaw, err := defaults.required("scale", "defaults")
	if err != nil {
		return nil, err
	}
	defaultScale, err := scaleFromV1(defaultScaleRaw, "defaults.scale")
	if err != nil {
		return nil, err
	}
	defaultChromaRaw, err := defaults.required("chroma", "defaults")
	if err != nil {
		return nil, err
	}
	defaultChroma, err := chromaFromV1(defaultChromaRaw, "defaults.chroma")
	if err != nil {
		return nil, err
	}
	defaultPacket, err := requiredInt(defaults, "max_packet_bytes", "defaults")
	if err != nil {
		return nil, err
	}
	if s.Domains, err = parseDomainsV1(root, s); err != nil {
		return nil, err
	}
	screensRaw, err := requiredArray(root, "screens", "$")
	if err != nil {
		return nil, err
	}
	for i, raw := range screensRaw {
		path := fmt.Sprintf("screens[%d]", i)
		o, err := asObject(raw, path)
		if err != nil {
			return nil, err
		}
		sc, traces, err := parseScreenV1(o, path, defaultScale, defaultChroma, defaultPacket, s.TickMillis, rounding)
		if err != nil {
			return nil, err
		}
		s.Screens = append(s.Screens, sc)
		s.Traces = append(s.Traces, traces...)
	}
	var eventTraces []ConversionTrace
	if s.Events, eventTraces, err = parseEventsV1(root, s.TickMillis, rounding); err != nil {
		return nil, err
	}
	s.Traces = append(s.Traces, eventTraces...)
	windows, traces, err := parseWindowsV1(root, s)
	if err != nil {
		return nil, err
	}
	s.Windows = windows
	s.Traces = append(s.Traces, traces...)
	return s, nil
}

func parseRounding(root jsonObj) (RoundingMode, error) {
	v, ok := root.optional("rounding")
	if !ok {
		return RoundFloor, nil
	}
	o, err := asObject(v, "rounding")
	if err != nil {
		return "", err
	}
	var mode = RoundFloor
	if v, ok := o.optional("time"); ok {
		s, err := asString(v, "rounding.time")
		if err != nil {
			return "", err
		}
		mode, ok = ParseRoundingMode(s)
		if !ok {
			return "", errf(CodeSchema, "rounding.time", "unknown rounding mode %q", s)
		}
	}
	if v, ok := o.optional("coord"); ok {
		s, err := asString(v, "rounding.coord")
		if err != nil {
			return "", err
		}
		if _, ok := ParseRoundingMode(s); !ok {
			return "", errf(CodeSchema, "rounding.coord", "unknown rounding mode %q", s)
		}
	}
	return mode, nil
}

func roundingOrDefault(mode RoundingMode) RoundingMode {
	if mode == "" {
		return RoundFloor
	}
	return mode
}

func optionalString(o jsonObj, key, path, def string) (string, error) {
	v, ok := o.optional(key)
	if !ok {
		return def, nil
	}
	return asString(v, path+"."+key)
}

func parseDomainsV1(root jsonObj, s *Session) ([]Domain, error) {
	arr, err := requiredArray(root, "domains", "$")
	if err != nil {
		return nil, err
	}
	out := make([]Domain, 0, len(arr))
	for i, raw := range arr {
		path := fmt.Sprintf("domains[%d]", i)
		o, err := asObject(raw, path)
		if err != nil {
			return nil, err
		}
		d := Domain{}
		if d.ID, err = requiredString(o, "id", path); err != nil {
			return nil, err
		}
		if d.Parent, err = optionalString(o, "parent", path, ""); err != nil {
			return nil, err
		}
		origin, err := requiredObject(o, "origin", path)
		if err != nil {
			return nil, err
		}
		if d.OriginX, err = requiredInt(origin, "x", path+".origin"); err != nil {
			return nil, err
		}
		if d.OriginY, err = requiredInt(origin, "y", path+".origin"); err != nil {
			return nil, err
		}
		s.Traces = append(s.Traces,
			trace(SchemaV1, path+".origin.x", nil, d.OriginX, "identity absolute coordinate", 0),
			trace(SchemaV1, path+".origin.y", nil, d.OriginY, "identity absolute coordinate", 0))
		out = append(out, d)
	}
	return out, nil
}

func parseScreenV1(o jsonObj, path string, fallbackScale Scale, fallbackChroma Chroma, fallbackPacket, tickMillis int64, mode RoundingMode) (Screen, []ConversionTrace, error) {
	traces := []ConversionTrace{}
	sc := Screen{}
	var err error
	if sc.ID, err = requiredString(o, "id", path); err != nil {
		return sc, nil, err
	}
	if sc.Name, err = optionalString(o, "name", path, sc.ID); err != nil {
		return sc, nil, err
	}
	if sc.Domain, err = requiredString(o, "domain", path); err != nil {
		return sc, nil, err
	}
	if sc.Master, err = optionalBool(o, "master", path, false); err != nil {
		return sc, nil, err
	}
	origin, err := requiredObject(o, "origin", path)
	if err != nil {
		return sc, nil, err
	}
	if sc.OriginX, err = requiredInt(origin, "x", path+".origin"); err != nil {
		return sc, nil, err
	}
	if sc.OriginY, err = requiredInt(origin, "y", path+".origin"); err != nil {
		return sc, nil, err
	}
	resolution, err := requiredObject(o, "resolution", path)
	if err != nil {
		return sc, nil, err
	}
	if sc.Width, err = requiredInt(resolution, "width", path+".resolution"); err != nil {
		return sc, nil, err
	}
	if sc.Height, err = requiredInt(resolution, "height", path+".resolution"); err != nil {
		return sc, nil, err
	}
	v, err := o.required("scale_profiles", path)
	if err != nil {
		return sc, nil, err
	}
	if sc.ScaleCapabilities, err = parseScaleList(v, SchemaV1, path+".scale_profiles"); err != nil {
		return sc, nil, err
	}
	v, err = o.required("chroma_modes", path)
	if err != nil {
		return sc, nil, err
	}
	if sc.ChromaCapabilities, err = parseChromaList(v, SchemaV1, path+".chroma_modes"); err != nil {
		return sc, nil, err
	}
	if v, ok := o.optional("default_scale"); ok {
		if sc.DefaultScale, err = scaleFromV1(v, path+".default_scale"); err != nil {
			return sc, nil, err
		}
		traces = append(traces, trace(SchemaV1, path+".default_scale", v, sc.DefaultScale.String(), "scale enum decode", 0))
	} else {
		sc.DefaultScale = fallbackScale
		traces = append(traces, trace(SchemaV1, path+".default_scale", nil, fallbackScale.String(), "fill from defaults.scale", 0))
	}
	if v, ok := o.optional("default_chroma"); ok {
		if sc.DefaultChroma, err = chromaFromV1(v, path+".default_chroma"); err != nil {
			return sc, nil, err
		}
		traces = append(traces, trace(SchemaV1, path+".default_chroma", v, string(sc.DefaultChroma), "chroma enum decode", 0))
	} else {
		sc.DefaultChroma = fallbackChroma
		traces = append(traces, trace(SchemaV1, path+".default_chroma", nil, string(fallbackChroma), "fill from defaults.chroma", 0))
	}
	if v, ok := o.optional("max_packet_bytes"); ok {
		if sc.MaxPacketBytes, err = asInt64(v, path+".max_packet_bytes"); err != nil {
			return sc, nil, err
		}
	} else {
		sc.MaxPacketBytes = fallbackPacket
		traces = append(traces, trace(SchemaV1, path+".max_packet_bytes", nil, fallbackPacket, "fill from defaults.max_packet_bytes", 0))
	}
	if !sc.SupportsScale(sc.DefaultScale) {
		return sc, nil, errf(CodeSchema, path+".default_scale", "default scale %s is not declared in scale_profiles", sc.DefaultScale)
	}
	if !sc.SupportsChroma(sc.DefaultChroma) {
		return sc, nil, errf(CodeSchema, path+".default_chroma", "default chroma %s is not declared in chroma_modes", sc.DefaultChroma)
	}
	for i, scale := range sc.ScaleCapabilities {
		traces = append(traces, trace(SchemaV1, fmt.Sprintf("%s.scale_profiles[%d]", path, i), nil, scale.String(), "scale enum decode", 0))
	}
	for i, chroma := range sc.ChromaCapabilities {
		traces = append(traces, trace(SchemaV1, fmt.Sprintf("%s.chroma_modes[%d]", path, i), nil, string(chroma), "chroma enum decode", 0))
	}
	traces = append(traces,
		trace(SchemaV1, path+".origin.x", nil, sc.OriginX, "identity absolute coordinate", 0),
		trace(SchemaV1, path+".origin.y", nil, sc.OriginY, "identity absolute coordinate", 0),
		trace(SchemaV1, path+".resolution.width", nil, sc.Width, "identity resolution", 0),
		trace(SchemaV1, path+".resolution.height", nil, sc.Height, "identity resolution", 0))
	_ = tickMillis
	_ = mode
	return sc, traces, nil
}

func parseEventsV1(root jsonObj, tickMillis int64, mode RoundingMode) ([]Event, []ConversionTrace, error) {
	arr, err := requiredArray(root, "events", "$")
	if err != nil {
		return nil, nil, err
	}
	out := make([]Event, 0, len(arr))
	traces := []ConversionTrace{}
	for i, raw := range arr {
		path := fmt.Sprintf("events[%d]", i)
		o, err := asObject(raw, path)
		if err != nil {
			return nil, nil, err
		}
		ev := Event{}
		if ev.ID, err = requiredString(o, "id", path); err != nil {
			return nil, nil, err
		}
		kindRaw, err := o.required("kind", path)
		if err != nil {
			return nil, nil, err
		}
		kind, err := asString(kindRaw, path+".kind")
		if err != nil {
			return nil, nil, err
		}
		if ev.Kind, err = parseEventKind(kind, path+".kind"); err != nil {
			return nil, nil, err
		}
		if ev.ScreenID, err = optionalString(o, "screen_id", path, ""); err != nil {
			return nil, nil, err
		}
		if ev.Domain, err = optionalString(o, "domain", path, ""); err != nil {
			return nil, nil, err
		}
		if ev.FromDomain, err = optionalString(o, "from_domain", path, ""); err != nil {
			return nil, nil, err
		}
		if ev.ToDomain, err = optionalString(o, "to_domain", path, ""); err != nil {
			return nil, nil, err
		}
		if ev.AtTick, err = requiredInt(o, "at_tick", path); err != nil {
			return nil, nil, err
		}
		if v, ok := o.optional("timeout_ticks"); ok {
			if ev.TimeoutTicks, err = asInt64(v, path+".timeout_ticks"); err != nil {
				return nil, nil, err
			}
		}
		out = append(out, ev)
		traces = append(traces,
			trace(SchemaV1, path+".kind", kindRaw, string(ev.Kind), "event kind decode", 0),
			trace(SchemaV1, path+".at_tick", nil, ev.AtTick, "identity tick", 0))
	}
	_ = tickMillis
	_ = mode
	return out, traces, nil
}

func parseWindowsV1(root jsonObj, s *Session) ([]Window, []ConversionTrace, error) {
	arr, err := requiredArray(root, "windows", "$")
	if err != nil {
		return nil, nil, err
	}
	out := make([]Window, 0, len(arr))
	traces := []ConversionTrace{}
	for i, raw := range arr {
		path := fmt.Sprintf("windows[%d]", i)
		o, err := asObject(raw, path)
		if err != nil {
			return nil, nil, err
		}
		w := Window{}
		if w.ID, err = requiredString(o, "id", path); err != nil {
			return nil, nil, err
		}
		index, err := requiredInt(o, "index", path)
		if err != nil {
			return nil, nil, err
		}
		if index > int64(^uint(0)>>1) {
			return nil, nil, &Error{Code: CodeOverflow, Path: path + ".index", Operands: []int64{index}}
		}
		w.Index = int(index)
		if w.StartTick, err = requiredInt(o, "start_tick", path); err != nil {
			return nil, nil, err
		}
		if w.EndTick, err = requiredInt(o, "end_tick", path); err != nil {
			return nil, nil, err
		}
		region, err := requiredObject(o, "region", path)
		if err != nil {
			return nil, nil, err
		}
		if w.Region, err = parseRegionV1(region, path+".region"); err != nil {
			return nil, nil, err
		}
		scaleRaw, err := o.required("scale", path)
		if err != nil {
			return nil, nil, err
		}
		if w.Scale, err = scaleFromV1(scaleRaw, path+".scale"); err != nil {
			return nil, nil, err
		}
		chromaRaw, err := o.required("chroma", path)
		if err != nil {
			return nil, nil, err
		}
		if w.Chroma, err = chromaFromV1(chromaRaw, path+".chroma"); err != nil {
			return nil, nil, err
		}
		if w.MaxPacketBytes, err = requiredInt(o, "max_packet_bytes", path); err != nil {
			return nil, nil, err
		}
		traces = append(traces,
			trace(SchemaV1, path+".region", region, w.Region, "identity absolute coordinate", 0),
			trace(SchemaV1, path+".start_tick", nil, w.StartTick, "identity tick", 0),
			trace(SchemaV1, path+".end_tick", nil, w.EndTick, "identity tick", 0),
			trace(SchemaV1, path+".scale", scaleRaw, w.Scale.String(), "scale enum decode", 0),
			trace(SchemaV1, path+".chroma", chromaRaw, string(w.Chroma), "chroma enum decode", 0))
		out = append(out, w)
	}
	_ = s
	return out, traces, nil
}

func parseRegionV1(o jsonObj, path string) (Region, error) {
	var r Region
	var err error
	if r.X, err = requiredInt(o, "x", path); err != nil {
		return r, err
	}
	if r.Y, err = requiredInt(o, "y", path); err != nil {
		return r, err
	}
	if r.W, err = requiredInt(o, "width", path); err != nil {
		return r, err
	}
	if r.H, err = requiredInt(o, "height", path); err != nil {
		return r, err
	}
	if r.X2() < r.X || r.Y2() < r.Y {
		return r, &Error{Code: CodeOverflow, Path: path, Field: path,
			Operands: []int64{r.X, r.W, r.Y, r.H}, Message: "region edge overflows int64"}
	}
	return r, nil
}

func loadV2(root jsonObj) (*Session, error) {
	s := &Session{SourceVersion: SchemaV2}
	var err error
	session, err := requiredObject(root, "session", "$")
	if err != nil {
		return nil, err
	}
	if s.ID, err = requiredString(session, "id", "session"); err != nil {
		return nil, err
	}
	if s.AlgorithmVersion, err = optionalString(session, "algorithmVersion", "session", AlgorithmVersion); err != nil {
		return nil, err
	}
	if v, ok := session.optional("seed"); ok {
		if s.Seed, err = asInt64(v, "session.seed"); err != nil {
			return nil, err
		}
	}
	if s.MasterScreenID, err = requiredString(session, "masterScreenId", "session"); err != nil {
		return nil, err
	}
	timebase, err := requiredObject(root, "timebase", "$")
	if err != nil {
		return nil, err
	}
	if s.TickMillis, err = requiredInt(timebase, "tickMillis", "timebase"); err != nil {
		return nil, err
	}
	rounding, err := parseRounding(root)
	if err != nil {
		return nil, err
	}
	s.TimeRounding = rounding
	negMS, err := requiredInt(timebase, "negotiationTimeoutMillis", "timebase")
	if err != nil {
		return nil, err
	}
	var negResidual int64
	if s.NegotiationTimeoutTicks, negResidual, err = msToTicks("timebase.negotiationTimeoutMillis", negMS, s.TickMillis, rounding); err != nil {
		return nil, err
	}
	recMS, err := requiredInt(timebase, "reconnectTimeoutMillis", "timebase")
	if err != nil {
		return nil, err
	}
	var recResidual int64
	if s.ReconnectTimeoutTicks, recResidual, err = msToTicks("timebase.reconnectTimeoutMillis", recMS, s.TickMillis, rounding); err != nil {
		return nil, err
	}
	s.Traces = append(s.Traces,
		trace(SchemaV2, "timebase.tickMillis", nil, s.TickMillis, "identity tick duration", 0),
		trace(SchemaV2, "timebase.negotiationTimeoutMillis", negMS, s.NegotiationTimeoutTicks, "milliseconds to internal ticks", negResidual),
		trace(SchemaV2, "timebase.reconnectTimeoutMillis", recMS, s.ReconnectTimeoutTicks, "milliseconds to internal ticks", recResidual))
	if err := parseDomainsV2(root, s); err != nil {
		return nil, err
	}
	if err := parseScreensV2(root, s); err != nil {
		return nil, err
	}
	var eventTraces []ConversionTrace
	if s.Events, eventTraces, err = parseEventsV2(root, s.TickMillis, rounding); err != nil {
		return nil, err
	}
	s.Traces = append(s.Traces, eventTraces...)
	windows, traces, err := parseWindowsV2(root, s)
	if err != nil {
		return nil, err
	}
	s.Windows = windows
	s.Traces = append(s.Traces, traces...)
	return s, nil
}

func parseDomainsV2(root jsonObj, s *Session) error {
	arr, err := requiredArray(root, "topology", "$")
	if err != nil {
		return err
	}
	for i, raw := range arr {
		path := fmt.Sprintf("topology[%d]", i)
		o, err := asObject(raw, path)
		if err != nil {
			return err
		}
		d := Domain{}
		if d.ID, err = requiredString(o, "domain", path); err != nil {
			return err
		}
		if d.Parent, err = optionalString(o, "parent", path, ""); err != nil {
			return err
		}
		origin, err := requiredObject(o, "origin", path)
		if err != nil {
			return err
		}
		if d.OriginX, err = requiredInt(origin, "x", path+".origin"); err != nil {
			return err
		}
		if d.OriginY, err = requiredInt(origin, "y", path+".origin"); err != nil {
			return err
		}
		s.Domains = append(s.Domains, d)
		s.Traces = append(s.Traces,
			trace(SchemaV2, path+".origin.x", nil, d.OriginX, "identity absolute coordinate", 0),
			trace(SchemaV2, path+".origin.y", nil, d.OriginY, "identity absolute coordinate", 0))
	}
	return nil
}

func parseScreensV2(root jsonObj, s *Session) error {
	arr, err := requiredArray(root, "topology", "$")
	if err != nil {
		return err
	}
	for i, raw := range arr {
		groupPath := fmt.Sprintf("topology[%d]", i)
		group, err := asObject(raw, groupPath)
		if err != nil {
			return err
		}
		domainID, err := requiredString(group, "domain", groupPath)
		if err != nil {
			return err
		}
		screens, err := requiredArray(group, "screens", groupPath)
		if err != nil {
			return err
		}
		for j, rawScreen := range screens {
			path := fmt.Sprintf("%s.screens[%d]", groupPath, j)
			o, err := asObject(rawScreen, path)
			if err != nil {
				return err
			}
			sc, traces, err := parseScreenV2(o, path, domainID, s.TickMillis, RoundFloor)
			if err != nil {
				return err
			}
			s.Screens = append(s.Screens, sc)
			s.Traces = append(s.Traces, traces...)
		}
	}
	return nil
}

func parseScreenV2(o jsonObj, path, domainID string, tickMillis int64, mode RoundingMode) (Screen, []ConversionTrace, error) {
	traces := []ConversionTrace{}
	sc := Screen{Domain: domainID}
	var err error
	if sc.ID, err = requiredString(o, "screenId", path); err != nil {
		return sc, nil, err
	}
	if sc.Name, err = optionalString(o, "displayName", path, sc.ID); err != nil {
		return sc, nil, err
	}
	if sc.Master, err = optionalBool(o, "master", path, false); err != nil {
		return sc, nil, err
	}
	if sc.OriginX, err = requiredInt(o, "originX", path); err != nil {
		return sc, nil, err
	}
	if sc.OriginY, err = requiredInt(o, "originY", path); err != nil {
		return sc, nil, err
	}
	res, err := requiredObject(o, "resolution", path)
	if err != nil {
		return sc, nil, err
	}
	if sc.Width, err = requiredInt(res, "w", path+".resolution"); err != nil {
		return sc, nil, err
	}
	if sc.Height, err = requiredInt(res, "h", path+".resolution"); err != nil {
		return sc, nil, err
	}
	v, err := o.required("scaleProfiles", path)
	if err != nil {
		return sc, nil, err
	}
	if sc.ScaleCapabilities, err = parseScaleList(v, SchemaV2, path+".scaleProfiles"); err != nil {
		return sc, nil, err
	}
	v, err = o.required("chromaModes", path)
	if err != nil {
		return sc, nil, err
	}
	if sc.ChromaCapabilities, err = parseChromaList(v, SchemaV2, path+".chromaModes"); err != nil {
		return sc, nil, err
	}
	if v, ok := o.optional("defaultScale"); ok {
		if sc.DefaultScale, err = scaleFromV2(v, path+".defaultScale"); err != nil {
			return sc, nil, err
		}
		traces = append(traces, trace(SchemaV2, path+".defaultScale", v, sc.DefaultScale.String(), "scale enum decode", 0))
	} else {
		sc.DefaultScale = sc.ScaleCapabilities[0]
		traces = append(traces, trace(SchemaV2, path+".defaultScale", nil, sc.DefaultScale.String(), "fill from first scaleProfiles entry", 0))
	}
	if v, ok := o.optional("defaultChroma"); ok {
		if sc.DefaultChroma, err = chromaFromV2(v, path+".defaultChroma"); err != nil {
			return sc, nil, err
		}
		traces = append(traces, trace(SchemaV2, path+".defaultChroma", v, string(sc.DefaultChroma), "chroma enum decode", 0))
	} else {
		sc.DefaultChroma = sc.ChromaCapabilities[0]
		traces = append(traces, trace(SchemaV2, path+".defaultChroma", nil, string(sc.DefaultChroma), "fill from first chromaModes entry", 0))
	}
	if sc.MaxPacketBytes, err = requiredInt(o, "maxPacketBytes", path); err != nil {
		return sc, nil, err
	}
	for i, v := range sc.ScaleCapabilities {
		traces = append(traces, trace(SchemaV2, fmt.Sprintf("%s.scaleProfiles[%d]", path, i), nil, v.String(), "scale enum decode", 0))
	}
	for i, v := range sc.ChromaCapabilities {
		traces = append(traces, trace(SchemaV2, fmt.Sprintf("%s.chromaModes[%d]", path, i), nil, string(v), "chroma enum decode", 0))
	}
	traces = append(traces,
		trace(SchemaV2, path+".originX", nil, sc.OriginX, "identity absolute coordinate", 0),
		trace(SchemaV2, path+".originY", nil, sc.OriginY, "identity absolute coordinate", 0),
		trace(SchemaV2, path+".resolution.w", nil, sc.Width, "identity resolution", 0),
		trace(SchemaV2, path+".resolution.h", nil, sc.Height, "identity resolution", 0))
	_ = tickMillis
	_ = mode
	return sc, traces, nil
}

func parseEventsV2(root jsonObj, tickMillis int64, mode RoundingMode) ([]Event, []ConversionTrace, error) {
	arr, err := requiredArray(root, "events", "$")
	if err != nil {
		return nil, nil, err
	}
	out := make([]Event, 0, len(arr))
	traces := []ConversionTrace{}
	for i, raw := range arr {
		path := fmt.Sprintf("events[%d]", i)
		o, err := asObject(raw, path)
		if err != nil {
			return nil, nil, err
		}
		ev := Event{}
		if ev.ID, err = requiredString(o, "id", path); err != nil {
			return nil, nil, err
		}
		kindRaw, err := o.required("type", path)
		if err != nil {
			return nil, nil, err
		}
		kind, err := asString(kindRaw, path+".type")
		if err != nil {
			return nil, nil, err
		}
		if ev.Kind, err = parseEventKind(kind, path+".type"); err != nil {
			return nil, nil, err
		}
		if ev.ScreenID, err = optionalString(o, "screenId", path, ""); err != nil {
			return nil, nil, err
		}
		if ev.Domain, err = optionalString(o, "domain", path, ""); err != nil {
			return nil, nil, err
		}
		if ev.FromDomain, err = optionalString(o, "fromDomain", path, ""); err != nil {
			return nil, nil, err
		}
		if ev.ToDomain, err = optionalString(o, "toDomain", path, ""); err != nil {
			return nil, nil, err
		}
		ms, err := requiredInt(o, "atMillis", path)
		if err != nil {
			return nil, nil, err
		}
		var atResidual int64
		if ev.AtTick, atResidual, err = msToTicks(path+".atMillis", ms, tickMillis, mode); err != nil {
			return nil, nil, err
		}
		timeoutResidual := int64(0)
		if v, ok := o.optional("timeoutMillis"); ok {
			t, err := asInt64(v, path+".timeoutMillis")
			if err != nil {
				return nil, nil, err
			}
			if ev.TimeoutTicks, timeoutResidual, err = msToTicks(path+".timeoutMillis", t, tickMillis, mode); err != nil {
				return nil, nil, err
			}
			traces = append(traces, trace(SchemaV2, path+".timeoutMillis", t, ev.TimeoutTicks, "milliseconds to internal ticks", timeoutResidual))
		}
		out = append(out, ev)
		traces = append(traces,
			trace(SchemaV2, path+".type", kindRaw, string(ev.Kind), "event kind decode", 0),
			trace(SchemaV2, path+".atMillis", ms, ev.AtTick, "milliseconds to internal ticks", atResidual))
	}
	return out, traces, nil
}

func parseWindowsV2(root jsonObj, s *Session) ([]Window, []ConversionTrace, error) {
	arr, err := requiredArray(root, "windows", "$")
	if err != nil {
		return nil, nil, err
	}
	screenByID := map[string]Screen{}
	for _, sc := range s.Screens {
		screenByID[sc.ID] = sc
	}
	out := make([]Window, 0, len(arr))
	traces := []ConversionTrace{}
	for i, raw := range arr {
		path := fmt.Sprintf("windows[%d]", i)
		o, err := asObject(raw, path)
		if err != nil {
			return nil, nil, err
		}
		w := Window{}
		if w.ID, err = requiredString(o, "id", path); err != nil {
			return nil, nil, err
		}
		seq, err := requiredInt(o, "sequence", path)
		if err != nil {
			return nil, nil, err
		}
		if seq < 0 || seq > int64(^uint(0)>>1) {
			return nil, nil, errf(CodeSchema, path+".sequence", "out of range")
		}
		w.Index = int(seq)
		startMS, err := requiredInt(o, "startMillis", path)
		if err != nil {
			return nil, nil, err
		}
		var startResidual int64
		if w.StartTick, startResidual, err = msToTicks(path+".startMillis", startMS, s.TickMillis, roundingOrDefault(s.TimeRounding)); err != nil {
			return nil, nil, err
		}
		endMS, err := requiredInt(o, "endMillis", path)
		if err != nil {
			return nil, nil, err
		}
		var endResidual int64
		if w.EndTick, endResidual, err = msToTicks(path+".endMillis", endMS, s.TickMillis, roundingOrDefault(s.TimeRounding)); err != nil {
			return nil, nil, err
		}
		w.SourceTimeResidualMs = abs64(startResidual) + abs64(endResidual)
		region, err := requiredObject(o, "region", path)
		if err != nil {
			return nil, nil, err
		}
		originID, err := requiredString(region, "originScreenId", path+".region")
		if err != nil {
			return nil, nil, err
		}
		origin, ok := screenByID[originID]
		if !ok {
			return nil, nil, errf(CodeSchema, path+".region.originScreenId", "unknown screen %q", originID)
		}
		dx, err := requiredInt(region, "dx", path+".region")
		if err != nil {
			return nil, nil, err
		}
		dy, err := requiredInt(region, "dy", path+".region")
		if err != nil {
			return nil, nil, err
		}
		if dx < minOffset32 || dx > maxOffset32 || dy < minOffset32 || dy > maxOffset32 {
			return nil, nil, errf(CodeSchema, path+".region", "offset exceeds schema 2 signed 32-bit range")
		}
		w.Region.X, err = AddChecked(path+".region.x", origin.OriginX, dx)
		if err != nil {
			return nil, nil, err
		}
		w.Region.Y, err = AddChecked(path+".region.y", origin.OriginY, dy)
		if err != nil {
			return nil, nil, err
		}
		if w.Region.W, err = requiredInt(region, "dw", path+".region"); err != nil {
			return nil, nil, err
		}
		if w.Region.H, err = requiredInt(region, "dh", path+".region"); err != nil {
			return nil, nil, err
		}
		scaleRaw, err := o.required("scale", path)
		if err != nil {
			return nil, nil, err
		}
		if w.Scale, err = scaleFromV2(scaleRaw, path+".scale"); err != nil {
			return nil, nil, err
		}
		chromaRaw, err := o.required("chroma", path)
		if err != nil {
			return nil, nil, err
		}
		if w.Chroma, err = chromaFromV2(chromaRaw, path+".chroma"); err != nil {
			return nil, nil, err
		}
		if w.MaxPacketBytes, err = requiredInt(o, "maxPacketBytes", path); err != nil {
			return nil, nil, err
		}
		traces = append(traces,
			trace(SchemaV2, path+".startMillis", startMS, w.StartTick, "milliseconds to internal ticks", startResidual),
			trace(SchemaV2, path+".endMillis", endMS, w.EndTick, "milliseconds to internal ticks", endResidual),
			trace(SchemaV2, path+".region", region, w.Region, "reference-screen offset to absolute coordinate", 0),
			trace(SchemaV2, path+".scale", scaleRaw, w.Scale.String(), "scale enum decode", 0),
			trace(SchemaV2, path+".chroma", chromaRaw, string(w.Chroma), "chroma enum decode", 0))
		out = append(out, w)
	}
	return out, traces, nil
}

func loadV3(root jsonObj) (*Session, error) {
	s := &Session{SourceVersion: SchemaV3}
	var err error
	meta, err := requiredObject(root, "meta", "$")
	if err != nil {
		return nil, err
	}
	if s.ID, err = requiredString(meta, "sessionId", "meta"); err != nil {
		return nil, err
	}
	if s.AlgorithmVersion, err = optionalString(meta, "algorithmVersion", "meta", AlgorithmVersion); err != nil {
		return nil, err
	}
	if v, ok := meta.optional("seed"); ok {
		if s.Seed, err = asInt64(v, "meta.seed"); err != nil {
			return nil, err
		}
	}
	if s.MasterScreenID, err = requiredString(meta, "masterScreenId", "meta"); err != nil {
		return nil, err
	}
	tick, err := requiredObject(root, "tick", "$")
	if err != nil {
		return nil, err
	}
	if s.TickMillis, err = requiredInt(tick, "durationMs", "tick"); err != nil {
		return nil, err
	}
	rounding, err := parseRounding(root)
	if err != nil {
		return nil, err
	}
	s.TimeRounding = rounding
	neg, err := requiredObject(tick, "negotiationTimeout", "tick")
	if err != nil {
		return nil, err
	}
	negMS, err := parseTimeObject(neg, "tick.negotiationTimeout")
	if err != nil {
		return nil, err
	}
	var negResidual int64
	if s.NegotiationTimeoutTicks, negResidual, err = msToTicks("tick.negotiationTimeout", negMS, s.TickMillis, rounding); err != nil {
		return nil, err
	}
	rec, err := requiredObject(tick, "reconnectTimeout", "tick")
	if err != nil {
		return nil, err
	}
	recMS, err := parseTimeObject(rec, "tick.reconnectTimeout")
	if err != nil {
		return nil, err
	}
	var recResidual int64
	if s.ReconnectTimeoutTicks, recResidual, err = msToTicks("tick.reconnectTimeout", recMS, s.TickMillis, rounding); err != nil {
		return nil, err
	}
	s.Traces = append(s.Traces,
		trace(SchemaV3, "tick.negotiationTimeout", neg, s.NegotiationTimeoutTicks, "seconds+milliseconds to internal ticks", negResidual),
		trace(SchemaV3, "tick.reconnectTimeout", rec, s.ReconnectTimeoutTicks, "seconds+milliseconds to internal ticks", recResidual))
	topology, err := requiredObject(root, "topology", "$")
	if err != nil {
		return nil, err
	}
	roots, err := requiredArray(topology, "roots", "topology")
	if err != nil {
		return nil, err
	}
	for i, raw := range roots {
		if err := parseDomainTreeNode(raw, fmt.Sprintf("topology.roots[%d]", i), "", s); err != nil {
			return nil, err
		}
	}
	if len(s.Screens) == 0 {
		return nil, errf(CodeSchema, "topology.roots", "topology contains no screens")
	}
	var eventTraces []ConversionTrace
	if s.Events, eventTraces, err = parseEventsV3(root, s.TickMillis, rounding); err != nil {
		return nil, err
	}
	s.Traces = append(s.Traces, eventTraces...)
	windows, traces, err := parseWindowsV3(root, s)
	if err != nil {
		return nil, err
	}
	s.Windows = windows
	s.Traces = append(s.Traces, traces...)
	return s, nil
}

func parseDomainTreeNode(raw any, path, parent string, s *Session) error {
	o, err := asObject(raw, path)
	if err != nil {
		return err
	}
	d := Domain{Parent: parent}
	if d.ID, err = requiredString(o, "id", path); err != nil {
		return err
	}
	origin, err := requiredObject(o, "origin", path)
	if err != nil {
		return err
	}
	if d.OriginX, err = requiredInt(origin, "x", path+".origin"); err != nil {
		return err
	}
	if d.OriginY, err = requiredInt(origin, "y", path+".origin"); err != nil {
		return err
	}
	s.Domains = append(s.Domains, d)
	s.Traces = append(s.Traces,
		trace(SchemaV3, path+".origin.x", nil, d.OriginX, "identity absolute coordinate", 0),
		trace(SchemaV3, path+".origin.y", nil, d.OriginY, "identity absolute coordinate", 0))
	if screensRaw, ok := o.optional("screens"); ok {
		screens, err := asArray(screensRaw, path+".screens")
		if err != nil {
			return err
		}
		for i, rawScreen := range screens {
			screenPath := fmt.Sprintf("%s.screens[%d]", path, i)
			so, err := asObject(rawScreen, screenPath)
			if err != nil {
				return err
			}
			sc, traces, err := parseScreenV3(so, screenPath, d.ID)
			if err != nil {
				return err
			}
			s.Screens = append(s.Screens, sc)
			s.Traces = append(s.Traces, traces...)
		}
	}
	if childrenRaw, ok := o.optional("children"); ok {
		children, err := asArray(childrenRaw, path+".children")
		if err != nil {
			return err
		}
		for i, child := range children {
			if err := parseDomainTreeNode(child, fmt.Sprintf("%s.children[%d]", path, i), d.ID, s); err != nil {
				return err
			}
		}
	}
	return nil
}

func parseScreenV3(o jsonObj, path, domainID string) (Screen, []ConversionTrace, error) {
	traces := []ConversionTrace{}
	sc := Screen{Domain: domainID}
	var err error
	if sc.ID, err = requiredString(o, "id", path); err != nil {
		return sc, nil, err
	}
	if sc.Name, err = optionalString(o, "name", path, sc.ID); err != nil {
		return sc, nil, err
	}
	if sc.Master, err = optionalBool(o, "master", path, false); err != nil {
		return sc, nil, err
	}
	origin, err := requiredObject(o, "origin", path)
	if err != nil {
		return sc, nil, err
	}
	if sc.OriginX, err = requiredInt(origin, "x", path+".origin"); err != nil {
		return sc, nil, err
	}
	if sc.OriginY, err = requiredInt(origin, "y", path+".origin"); err != nil {
		return sc, nil, err
	}
	res, err := requiredObject(o, "resolution", path)
	if err != nil {
		return sc, nil, err
	}
	if sc.Width, err = requiredInt(res, "width", path+".resolution"); err != nil {
		return sc, nil, err
	}
	if sc.Height, err = requiredInt(res, "height", path+".resolution"); err != nil {
		return sc, nil, err
	}
	caps, err := requiredObject(o, "capabilities", path)
	if err != nil {
		return sc, nil, err
	}
	v, err := caps.required("scaleProfiles", path+".capabilities")
	if err != nil {
		return sc, nil, err
	}
	if sc.ScaleCapabilities, err = parseScaleList(v, SchemaV3, path+".capabilities.scaleProfiles"); err != nil {
		return sc, nil, err
	}
	v, err = caps.required("chromaModes", path+".capabilities")
	if err != nil {
		return sc, nil, err
	}
	if sc.ChromaCapabilities, err = parseChromaList(v, SchemaV3, path+".capabilities.chromaModes"); err != nil {
		return sc, nil, err
	}
	if v, ok := caps.optional("defaultScale"); ok {
		if sc.DefaultScale, err = scaleFromV3(v, path+".capabilities.defaultScale"); err != nil {
			return sc, nil, err
		}
		traces = append(traces, trace(SchemaV3, path+".capabilities.defaultScale", v, sc.DefaultScale.String(), "scale enum decode", 0))
	} else {
		sc.DefaultScale = sc.ScaleCapabilities[0]
		traces = append(traces, trace(SchemaV3, path+".capabilities.defaultScale", nil, sc.DefaultScale.String(), "fill from first scaleProfiles entry", 0))
	}
	if v, ok := caps.optional("defaultChroma"); ok {
		if sc.DefaultChroma, err = chromaFromV3(v, path+".capabilities.defaultChroma"); err != nil {
			return sc, nil, err
		}
		traces = append(traces, trace(SchemaV3, path+".capabilities.defaultChroma", v, string(sc.DefaultChroma), "chroma enum decode", 0))
	} else {
		sc.DefaultChroma = sc.ChromaCapabilities[0]
		traces = append(traces, trace(SchemaV3, path+".capabilities.defaultChroma", nil, string(sc.DefaultChroma), "fill from first chromaModes entry", 0))
	}
	if sc.MaxPacketBytes, err = requiredInt(o, "maxPacketBytes", path); err != nil {
		return sc, nil, err
	}
	for i, v := range sc.ScaleCapabilities {
		traces = append(traces, trace(SchemaV3, fmt.Sprintf("%s.capabilities.scaleProfiles[%d]", path, i), nil, v.String(), "scale enum decode", 0))
	}
	for i, v := range sc.ChromaCapabilities {
		traces = append(traces, trace(SchemaV3, fmt.Sprintf("%s.capabilities.chromaModes[%d]", path, i), nil, string(v), "chroma enum decode", 0))
	}
	traces = append(traces,
		trace(SchemaV3, path+".origin.x", nil, sc.OriginX, "identity absolute coordinate", 0),
		trace(SchemaV3, path+".origin.y", nil, sc.OriginY, "identity absolute coordinate", 0),
		trace(SchemaV3, path+".resolution.width", nil, sc.Width, "identity resolution", 0),
		trace(SchemaV3, path+".resolution.height", nil, sc.Height, "identity resolution", 0))
	return sc, traces, nil
}

func parseEventsV3(root jsonObj, tickMillis int64, mode RoundingMode) ([]Event, []ConversionTrace, error) {
	arr, err := requiredArray(root, "events", "$")
	if err != nil {
		return nil, nil, err
	}
	out := make([]Event, 0, len(arr))
	traces := []ConversionTrace{}
	for i, raw := range arr {
		path := fmt.Sprintf("events[%d]", i)
		o, err := asObject(raw, path)
		if err != nil {
			return nil, nil, err
		}
		ev := Event{}
		if ev.ID, err = requiredString(o, "id", path); err != nil {
			return nil, nil, err
		}
		kindRaw, err := o.required("kind", path)
		if err != nil {
			return nil, nil, err
		}
		kind, err := asString(kindRaw, path+".kind")
		if err != nil {
			return nil, nil, err
		}
		if ev.Kind, err = parseEventKind(kind, path+".kind"); err != nil {
			return nil, nil, err
		}
		if ev.ScreenID, err = optionalString(o, "screenId", path, ""); err != nil {
			return nil, nil, err
		}
		if ev.Domain, err = optionalString(o, "domain", path, ""); err != nil {
			return nil, nil, err
		}
		if ev.FromDomain, err = optionalString(o, "fromDomain", path, ""); err != nil {
			return nil, nil, err
		}
		if ev.ToDomain, err = optionalString(o, "toDomain", path, ""); err != nil {
			return nil, nil, err
		}
		at, err := requiredObject(o, "at", path)
		if err != nil {
			return nil, nil, err
		}
		ms, err := parseTimeObject(at, path+".at")
		if err != nil {
			return nil, nil, err
		}
		var atResidual int64
		if ev.AtTick, atResidual, err = msToTicks(path+".at", ms, tickMillis, mode); err != nil {
			return nil, nil, err
		}
		timeoutResidual := int64(0)
		if v, ok := o.optional("timeout"); ok {
			to, err := asObject(v, path+".timeout")
			if err != nil {
				return nil, nil, err
			}
			tms, err := parseTimeObject(to, path+".timeout")
			if err != nil {
				return nil, nil, err
			}
			if ev.TimeoutTicks, timeoutResidual, err = msToTicks(path+".timeout", tms, tickMillis, mode); err != nil {
				return nil, nil, err
			}
			traces = append(traces, trace(SchemaV3, path+".timeout", to, ev.TimeoutTicks, "seconds+milliseconds to internal ticks", timeoutResidual))
		}
		out = append(out, ev)
		traces = append(traces,
			trace(SchemaV3, path+".kind", kindRaw, string(ev.Kind), "event kind decode", 0),
			trace(SchemaV3, path+".at", at, ev.AtTick, "seconds+milliseconds to internal ticks", atResidual))
	}
	return out, traces, nil
}

func parseEventKind(s, path string) (EventKind, error) {
	switch s {
	case "join", "JOIN":
		return EventJoin, nil
	case "leave", "LEAVE":
		return EventLeave, nil
	case "topology", "TOPOLOGY":
		return EventTopology, nil
	case "negotiate", "NEGOTIATE":
		return EventNegotiate, nil
	default:
		return "", errf(CodeSchema, path, "unknown event kind %q", s)
	}
}

func parseWindowsV3(root jsonObj, s *Session) ([]Window, []ConversionTrace, error) {
	arr, err := requiredArray(root, "windows", "$")
	if err != nil {
		return nil, nil, err
	}
	domainByID := map[string]Domain{}
	for _, d := range s.Domains {
		domainByID[d.ID] = d
	}
	screenByID := map[string]Screen{}
	for _, sc := range s.Screens {
		screenByID[sc.ID] = sc
	}
	out := make([]Window, 0, len(arr))
	traces := []ConversionTrace{}
	for i, raw := range arr {
		path := fmt.Sprintf("windows[%d]", i)
		o, err := asObject(raw, path)
		if err != nil {
			return nil, nil, err
		}
		w := Window{}
		if w.ID, err = requiredString(o, "id", path); err != nil {
			return nil, nil, err
		}
		seq, err := requiredInt(o, "index", path)
		if err != nil {
			return nil, nil, err
		}
		if seq < 0 || seq > int64(^uint(0)>>1) {
			return nil, nil, errf(CodeSchema, path+".index", "out of range")
		}
		w.Index = int(seq)
		timeObj, err := requiredObject(o, "time", path)
		if err != nil {
			return nil, nil, err
		}
		startObj, err := requiredObject(timeObj, "start", path+".time")
		if err != nil {
			return nil, nil, err
		}
		startMS, err := parseTimeObject(startObj, path+".time.start")
		if err != nil {
			return nil, nil, err
		}
		var startResidual int64
		if w.StartTick, startResidual, err = msToTicks(path+".time.start", startMS, s.TickMillis, roundingOrDefault(s.TimeRounding)); err != nil {
			return nil, nil, err
		}
		endObj, err := requiredObject(timeObj, "end", path+".time")
		if err != nil {
			return nil, nil, err
		}
		endMS, err := parseTimeObject(endObj, path+".time.end")
		if err != nil {
			return nil, nil, err
		}
		var endResidual int64
		if w.EndTick, endResidual, err = msToTicks(path+".time.end", endMS, s.TickMillis, roundingOrDefault(s.TimeRounding)); err != nil {
			return nil, nil, err
		}
		w.SourceTimeResidualMs = abs64(startResidual) + abs64(endResidual)
		region, err := requiredObject(o, "region", path)
		if err != nil {
			return nil, nil, err
		}
		originScreenID, err := optionalString(region, "originScreenId", path+".region", "")
		if err != nil {
			return nil, nil, err
		}
		originX, originY := int64(0), int64(0)
		originRule := ""
		if originScreenID != "" {
			origin, ok := screenByID[originScreenID]
			if !ok {
				return nil, nil, errf(CodeSchema, path+".region.originScreenId", "unknown screen %q", originScreenID)
			}
			originX, originY = origin.OriginX, origin.OriginY
			originRule = "screen-origin offset to absolute coordinate"
		} else {
			domainID, err := requiredString(region, "domain", path+".region")
			if err != nil {
				return nil, nil, err
			}
			domain, ok := domainByID[domainID]
			if !ok {
				return nil, nil, errf(CodeSchema, path+".region.domain", "unknown domain %q", domainID)
			}
			originX, originY = domain.OriginX, domain.OriginY
			originRule = "domain-origin offset to absolute coordinate"
		}
		dx, err := requiredInt(region, "dx", path+".region")
		if err != nil {
			return nil, nil, err
		}
		dy, err := requiredInt(region, "dy", path+".region")
		if err != nil {
			return nil, nil, err
		}
		if dx < minOffset32 || dx > maxOffset32 || dy < minOffset32 || dy > maxOffset32 {
			return nil, nil, errf(CodeSchema, path+".region", "offset exceeds schema 3 signed 32-bit range")
		}
		w.Region.X, err = AddChecked(path+".region.x", originX, dx)
		if err != nil {
			return nil, nil, err
		}
		w.Region.Y, err = AddChecked(path+".region.y", originY, dy)
		if err != nil {
			return nil, nil, err
		}
		if w.Region.W, err = requiredInt(region, "w", path+".region"); err != nil {
			return nil, nil, err
		}
		if w.Region.H, err = requiredInt(region, "h", path+".region"); err != nil {
			return nil, nil, err
		}
		scaleRaw, err := o.required("scale", path)
		if err != nil {
			return nil, nil, err
		}
		if w.Scale, err = scaleFromV3(scaleRaw, path+".scale"); err != nil {
			return nil, nil, err
		}
		chromaRaw, err := o.required("chroma", path)
		if err != nil {
			return nil, nil, err
		}
		if w.Chroma, err = chromaFromV3(chromaRaw, path+".chroma"); err != nil {
			return nil, nil, err
		}
		if w.MaxPacketBytes, err = requiredInt(o, "maxPacketBytes", path); err != nil {
			return nil, nil, err
		}
		traces = append(traces,
			trace(SchemaV3, path+".time.start", startObj, w.StartTick, "seconds+milliseconds to internal ticks", startResidual),
			trace(SchemaV3, path+".time.end", endObj, w.EndTick, "seconds+milliseconds to internal ticks", endResidual),
			trace(SchemaV3, path+".region", region, w.Region, originRule, 0),
			trace(SchemaV3, path+".scale", scaleRaw, w.Scale.String(), "scale enum decode", 0),
			trace(SchemaV3, path+".chroma", chromaRaw, string(w.Chroma), "chroma enum decode", 0))
		out = append(out, w)
	}
	return out, traces, nil
}

// SortedScreenIDs is a small helper used by deterministic report generation.
func SortedScreenIDs(s *Session) []string {
	ids := make([]string, 0, len(s.Screens))
	for _, sc := range s.Screens {
		ids = append(ids, sc.ID)
	}
	sort.Strings(ids)
	return ids
}

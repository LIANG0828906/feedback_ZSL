package schema

import (
	"fmt"

	"obsproj/internal/model"
)

type v3Screen struct {
	ID         string       `json:"sid"`
	Name       string       `json:"sn"`
	OriginX    int64        `json:"ox"`
	OriginY    int64        `json:"oy"`
	Width      int64        `json:"pw"`
	Height     int64        `json:"ph"`
	Scale      int64        `json:"sc"`
	Chroma     int64        `json:"ch"`
	Capability v3Capability `json:"cap"`
}

type v3Capability struct {
	Scales        []int64 `json:"scs"`
	Chromas       []int64 `json:"chs"`
	MaxPacketSize int     `json:"mps"`
}

type v3Topology struct {
	OriginX           *int64   `json:"ox,omitempty"`
	OriginY           *int64   `json:"oy,omitempty"`
	Width             *int64   `json:"pw,omitempty"`
	Height            *int64   `json:"ph,omitempty"`
	Scale             *int64   `json:"sc,omitempty"`
	Chroma            *int64   `json:"ch,omitempty"`
	CapabilityScales  *[]int64 `json:"scs,omitempty"`
	CapabilityChromas *[]int64 `json:"chs,omitempty"`
	MaxPacketSize     *int     `json:"mps,omitempty"`
}

type v3Event struct {
	EventID  string      `json:"eid"`
	WindowNo int         `json:"wn"`
	AtTick   int64       `json:"tt"`
	Kind     string      `json:"k"`
	ScreenID string      `json:"sid"`
	Topology *v3Topology `json:"topo,omitempty"`
}

type v3Region struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
	W int64 `json:"w"`
	H int64 `json:"h"`
}

type v3Frame struct {
	WindowNo int      `json:"wn"`
	AtTick   int64    `json:"tt"`
	ScreenID string   `json:"sid"`
	Region   v3Region `json:"r"`
}

type v3Session struct {
	SchemaVersion string     `json:"schema_version"`
	SessionID     string     `json:"session"`
	Seed          int64      `json:"seed"`
	TickRate      int64      `json:"tick_rate"`
	Screens       []v3Screen `json:"screens"`
	Events        []v3Event  `json:"events"`
	Frames        []v3Frame  `json:"frames"`
}

func decodeV3(root object) (*model.Session, *model.KernelError) {
	const version = Version3
	sessionID, err := stringField(root, "session", version, "session_id", "session")
	if err != nil {
		return nil, err
	}
	seed, err := intField(root, "seed", version, "seed", "seed")
	if err != nil {
		return nil, err
	}
	tickRate, err := intField(root, "tick_rate", version, "tick_rate", "tick_rate")
	if err != nil {
		return nil, err
	}
	internalRate, ovf := model.InternalTickRate(tickRate)
	if ovf != nil {
		ovf.Version = version
		return nil, ovf
	}
	session := &model.Session{
		ID:            sessionID,
		Seed:          seed,
		TickRate:      tickRate,
		InternalRate:  internalRate,
		SourceVersion: version,
	}

	screenRaw, err := arrayField(root, "screens", version, "screens", "screens")
	if err != nil {
		return nil, err
	}
	for i, raw := range screenRaw {
		base := fmt.Sprintf("screens[%d]", i)
		obj, objErr := asObject(raw, version, base, base)
		if objErr != nil {
			return nil, objErr
		}
		id, ferr := stringField(obj, "sid", version, base+".sid", "sid")
		if ferr != nil {
			return nil, ferr
		}
		name, ferr := stringField(obj, "sn", version, base+".sn", "sn")
		if ferr != nil {
			return nil, ferr
		}
		ox, ferr := intField(obj, "ox", version, base+".ox", "ox")
		if ferr != nil {
			return nil, ferr
		}
		oy, ferr := intField(obj, "oy", version, base+".oy", "oy")
		if ferr != nil {
			return nil, ferr
		}
		pw, ferr := intField(obj, "pw", version, base+".pw", "pw")
		if ferr != nil {
			return nil, ferr
		}
		ph, ferr := intField(obj, "ph", version, base+".ph", "ph")
		if ferr != nil {
			return nil, ferr
		}
		originX, ovf := model.UnitsToInternal(base+".ox", ox)
		if ovf != nil {
			ovf.Version = version
			return nil, ovf
		}
		originY, ovf := model.UnitsToInternal(base+".oy", oy)
		if ovf != nil {
			ovf.Version = version
			return nil, ovf
		}
		scaleOrdinal, ferr := intField(obj, "sc", version, base+".sc", "sc")
		if ferr != nil {
			return nil, ferr
		}
		scale, ferr := v3ScaleFromOrdinal(scaleOrdinal, version, base+".sc")
		if ferr != nil {
			return nil, ferr
		}
		chromaOrdinal, ferr := intField(obj, "ch", version, base+".ch", "ch")
		if ferr != nil {
			return nil, ferr
		}
		chroma, ferr := v3ChromaFromOrdinal(chromaOrdinal, version, base+".ch")
		if ferr != nil {
			return nil, ferr
		}
		capObj, ferr := objectField(obj, "cap", version, base+".cap", "cap")
		if ferr != nil {
			return nil, ferr
		}
		capScales, ferr := decodeV3ScaleList(capObj, "scs", version, base+".cap.scs")
		if ferr != nil {
			return nil, ferr
		}
		capChromas, ferr := decodeV3ChromaList(capObj, "chs", version, base+".cap.chs")
		if ferr != nil {
			return nil, ferr
		}
		maxPacket, ferr := intField(capObj, "mps", version, base+".cap.mps", "mps")
		if ferr != nil {
			return nil, ferr
		}
		if maxPacket <= 0 || maxPacket > int64(^uint(0)>>1) {
			return nil, schemaError(version, base+".cap.mps", "mps", "invalid packet size")
		}
		session.Screens = append(session.Screens, model.Screen{
			ID:         id,
			Name:       name,
			Origin:     model.Point{X: originX, Y: originY},
			Resolution: model.Size{W: pw, H: ph},
			Scale:      scale,
			Chroma:     chroma,
			Capability: model.Capability{
				Scales:        capScales,
				Chromas:       capChromas,
				MaxPacketSize: int(maxPacket),
			},
		})
	}

	eventRaw, err := arrayField(root, "events", version, "events", "events")
	if err != nil {
		return nil, err
	}
	for i, raw := range eventRaw {
		base := fmt.Sprintf("events[%d]", i)
		obj, objErr := asObject(raw, version, base, base)
		if objErr != nil {
			return nil, objErr
		}
		id, ferr := stringField(obj, "eid", version, base+".eid", "eid")
		if ferr != nil {
			return nil, ferr
		}
		window, ferr := intField(obj, "wn", version, base+".wn", "wn")
		if ferr != nil {
			return nil, ferr
		}
		atTick, ferr := intField(obj, "tt", version, base+".tt", "tt")
		if ferr != nil {
			return nil, ferr
		}
		at, ovf := model.NativeTicksToInternal(base+".tt", atTick, tickRate, internalRate)
		if ovf != nil {
			ovf.Version = version
			return nil, ovf
		}
		kindText, ferr := stringField(obj, "k", version, base+".k", "k")
		if ferr != nil {
			return nil, ferr
		}
		kind := model.EventKind(kindText)
		if kind != model.EventJoin && kind != model.EventExit && kind != model.EventTopology {
			return nil, schemaError(version, base+".k", "k", "unknown event kind")
		}
		screenID, ferr := stringField(obj, "sid", version, base+".sid", "sid")
		if ferr != nil {
			return nil, ferr
		}
		var patch *model.TopologyPatch
		topo, present, ferr := optionalObject(obj, "topo", version, base+".topo", "topo")
		if ferr != nil {
			return nil, ferr
		}
		if present || kind == model.EventTopology {
			if !present {
				return nil, schemaError(version, base+".topo", "topo", "topology patch is required")
			}
			patch, ferr = decodeV3Topology(topo, version, base+".topo")
			if ferr != nil {
				return nil, ferr
			}
		}
		session.Events = append(session.Events, model.Event{
			ID:       id,
			Window:   int(window),
			At:       at,
			Kind:     kind,
			ScreenID: screenID,
			Topology: patch,
		})
	}

	frameRaw, err := arrayField(root, "frames", version, "frames", "frames")
	if err != nil {
		return nil, err
	}
	for i, raw := range frameRaw {
		base := fmt.Sprintf("frames[%d]", i)
		obj, objErr := asObject(raw, version, base, base)
		if objErr != nil {
			return nil, objErr
		}
		window, ferr := intField(obj, "wn", version, base+".wn", "wn")
		if ferr != nil {
			return nil, ferr
		}
		atTick, ferr := intField(obj, "tt", version, base+".tt", "tt")
		if ferr != nil {
			return nil, ferr
		}
		at, ovf := model.NativeTicksToInternal(base+".tt", atTick, tickRate, internalRate)
		if ovf != nil {
			ovf.Version = version
			return nil, ovf
		}
		screenID, ferr := stringField(obj, "sid", version, base+".sid", "sid")
		if ferr != nil {
			return nil, ferr
		}
		regionObj, ferr := objectField(obj, "r", version, base+".r", "r")
		if ferr != nil {
			return nil, ferr
		}
		region, ferr := decodeV3Region(regionObj, version, base+".r")
		if ferr != nil {
			return nil, ferr
		}
		session.Commands = append(session.Commands, model.Command{
			Window:   int(window),
			At:       at,
			ScreenID: screenID,
			Region:   region,
		})
	}
	return session, nil
}

func decodeV3ScaleList(obj object, key, version, field string) ([]model.Scale, *model.KernelError) {
	raws, err := arrayField(obj, key, version, field, key)
	if err != nil {
		return nil, err
	}
	out := make([]model.Scale, 0, len(raws))
	for i, raw := range raws {
		value, intErr := parseInt(raw, version, fmt.Sprintf("%s[%d]", field, i), key)
		if intErr != nil {
			return nil, intErr
		}
		scale, scaleErr := v3ScaleFromOrdinal(value, version, fmt.Sprintf("%s[%d]", field, i))
		if scaleErr != nil {
			return nil, scaleErr
		}
		out = append(out, scale)
	}
	return out, nil
}

func decodeV3ChromaList(obj object, key, version, field string) ([]model.Chroma, *model.KernelError) {
	raws, err := arrayField(obj, key, version, field, key)
	if err != nil {
		return nil, err
	}
	out := make([]model.Chroma, 0, len(raws))
	for i, raw := range raws {
		value, intErr := parseInt(raw, version, fmt.Sprintf("%s[%d]", field, i), key)
		if intErr != nil {
			return nil, intErr
		}
		chroma, chromaErr := v3ChromaFromOrdinal(value, version, fmt.Sprintf("%s[%d]", field, i))
		if chromaErr != nil {
			return nil, chromaErr
		}
		out = append(out, chroma)
	}
	return out, nil
}

func decodeV3Topology(obj object, version, base string) (*model.TopologyPatch, *model.KernelError) {
	patch := &model.TopologyPatch{}
	_, hasOriginX := obj["ox"]
	_, hasOriginY := obj["oy"]
	if hasOriginX != hasOriginY {
		return nil, schemaError(version, base+".ox", "ox/oy", "ox and oy must be supplied together")
	}
	_, hasWidth := obj["pw"]
	_, hasHeight := obj["ph"]
	if hasWidth != hasHeight {
		return nil, schemaError(version, base+".pw", "pw/ph", "pw and ph must be supplied together")
	}
	if raw, ok := obj["ox"]; ok {
		v, err := parseInt(raw, version, base+".ox", "ox")
		if err != nil {
			return nil, err
		}
		micro, ovf := model.UnitsToInternal(base+".ox", v)
		if ovf != nil {
			ovf.Version = version
			return nil, ovf
		}
		patch.Origin = &model.Point{X: micro}
	}
	if raw, ok := obj["oy"]; ok {
		v, err := parseInt(raw, version, base+".oy", "oy")
		if err != nil {
			return nil, err
		}
		micro, ovf := model.UnitsToInternal(base+".oy", v)
		if ovf != nil {
			ovf.Version = version
			return nil, ovf
		}
		if patch.Origin == nil {
			patch.Origin = &model.Point{}
		}
		patch.Origin.Y = micro
	}
	if raw, ok := obj["pw"]; ok {
		v, err := parseInt(raw, version, base+".pw", "pw")
		if err != nil {
			return nil, err
		}
		if patch.Resolution == nil {
			patch.Resolution = &model.Size{}
		}
		patch.Resolution.W = v
	}
	if raw, ok := obj["ph"]; ok {
		v, err := parseInt(raw, version, base+".ph", "ph")
		if err != nil {
			return nil, err
		}
		if patch.Resolution == nil {
			patch.Resolution = &model.Size{}
		}
		patch.Resolution.H = v
	}
	if raw, ok := obj["sc"]; ok {
		v, err := parseInt(raw, version, base+".sc", "sc")
		if err != nil {
			return nil, err
		}
		scale, scaleErr := v3ScaleFromOrdinal(v, version, base+".sc")
		if scaleErr != nil {
			return nil, scaleErr
		}
		patch.Scale = &scale
	}
	if raw, ok := obj["ch"]; ok {
		v, err := parseInt(raw, version, base+".ch", "ch")
		if err != nil {
			return nil, err
		}
		chroma, chromaErr := v3ChromaFromOrdinal(v, version, base+".ch")
		if chromaErr != nil {
			return nil, chromaErr
		}
		patch.Chroma = &chroma
	}
	if _, ok := obj["scs"]; ok {
		scales, err := decodeV3ScaleList(obj, "scs", version, base+".scs")
		if err != nil {
			return nil, err
		}
		patch.CapabilityScales = &scales
	}
	if _, ok := obj["chs"]; ok {
		chromas, err := decodeV3ChromaList(obj, "chs", version, base+".chs")
		if err != nil {
			return nil, err
		}
		patch.CapabilityChromas = &chromas
	}
	if raw, ok := obj["mps"]; ok {
		v, err := parseInt(raw, version, base+".mps", "mps")
		if err != nil {
			return nil, err
		}
		if v <= 0 || v > int64(^uint(0)>>1) {
			return nil, schemaError(version, base+".mps", "mps", "invalid packet size")
		}
		n := int(v)
		patch.MaxPacketSize = &n
	}
	return patch, nil
}

func decodeV3Region(obj object, version, base string) (model.Rect, *model.KernelError) {
	var out model.Rect
	values := []struct {
		key    string
		target *int64
	}{
		{"x", &out.X},
		{"y", &out.Y},
		{"w", &out.W},
		{"h", &out.H},
	}
	for _, item := range values {
		v, err := intField(obj, item.key, version, base+"."+item.key, item.key)
		if err != nil {
			return model.Rect{}, err
		}
		micro, ovf := model.UnitsToInternal(base+"."+item.key, v)
		if ovf != nil {
			ovf.Version = version
			return model.Rect{}, ovf
		}
		*item.target = micro
	}
	return out, nil
}

func encodeV3(session *model.Session) ([]byte, []model.Loss, error) {
	var losses []model.Loss
	out := v3Session{
		SchemaVersion: Version3,
		SessionID:     session.ID,
		Seed:          session.Seed,
		TickRate:      session.TickRate,
	}
	for i := range session.Screens {
		screen := &session.Screens[i]
		base := fmt.Sprintf("screens[%d]", i)
		ox, residual, err := model.InternalToUnits(base+".ox", screen.Origin.X)
		if err != nil {
			return nil, nil, err
		}
		if residual != 0 {
			losses = append(losses, floorLoss(base+".ox", screen.Origin.X, ox, "internal logical unit", Version3))
		}
		oy, residual, err := model.InternalToUnits(base+".oy", screen.Origin.Y)
		if err != nil {
			return nil, nil, err
		}
		if residual != 0 {
			losses = append(losses, floorLoss(base+".oy", screen.Origin.Y, oy, "internal logical unit", Version3))
		}
		capScales := make([]int64, 0, len(screen.Capability.Scales))
		for _, scale := range screen.Capability.Scales {
			capScales = append(capScales, v3ScaleOrdinal[scale])
		}
		capChromas := make([]int64, 0, len(screen.Capability.Chromas))
		for _, chroma := range screen.Capability.Chromas {
			capChromas = append(capChromas, v3ChromaOrdinal[chroma])
		}
		out.Screens = append(out.Screens, v3Screen{
			ID:      screen.ID,
			Name:    screen.Name,
			OriginX: ox,
			OriginY: oy,
			Width:   screen.Resolution.W,
			Height:  screen.Resolution.H,
			Scale:   v3ScaleOrdinal[screen.Scale],
			Chroma:  v3ChromaOrdinal[screen.Chroma],
			Capability: v3Capability{
				Scales:        capScales,
				Chromas:       capChromas,
				MaxPacketSize: screen.Capability.MaxPacketSize,
			},
		})
	}
	for i := range session.Events {
		event := &session.Events[i]
		base := fmt.Sprintf("events[%d]", i)
		ticks, residual, err := model.InternalToNativeTicks(event.At, session.InternalRate, session.TickRate)
		if err != nil {
			return nil, nil, err
		}
		if residual != 0 {
			losses = append(losses, floorLoss(base+".tt", event.At, ticks, "native tick", Version3))
		}
		native := v3Event{
			EventID:  event.ID,
			WindowNo: event.Window,
			AtTick:   ticks,
			Kind:     string(event.Kind),
			ScreenID: event.ScreenID,
		}
		if event.Topology != nil {
			topo, topoLosses, encErr := encodeV3Topology(event.Topology, base+".topo", &losses)
			if encErr != nil {
				return nil, nil, encErr
			}
			native.Topology = topo
			losses = topoLosses
		}
		out.Events = append(out.Events, native)
	}
	for i := range session.Commands {
		command := &session.Commands[i]
		base := fmt.Sprintf("frames[%d]", i)
		ticks, residual, err := model.InternalToNativeTicks(command.At, session.InternalRate, session.TickRate)
		if err != nil {
			return nil, nil, err
		}
		if residual != 0 {
			losses = append(losses, floorLoss(base+".tt", command.At, ticks, "native tick", Version3))
		}
		region, regionLosses, encErr := encodeV3Region(command.Region, base+".r", &losses)
		if encErr != nil {
			return nil, nil, encErr
		}
		losses = regionLosses
		out.Frames = append(out.Frames, v3Frame{
			WindowNo: command.Window,
			AtTick:   ticks,
			ScreenID: command.ScreenID,
			Region:   region,
		})
	}
	data, err := marshal(out)
	return data, losses, err
}

func encodeV3Topology(patch *model.TopologyPatch, base string, losses *[]model.Loss) (*v3Topology, []model.Loss, error) {
	out := &v3Topology{}
	if patch.Origin != nil {
		ox, residual, err := model.InternalToUnits(base+".ox", patch.Origin.X)
		if err != nil {
			return nil, nil, err
		}
		if residual != 0 {
			*losses = append(*losses, floorLoss(base+".ox", patch.Origin.X, ox, "internal logical unit", Version3))
		}
		oy, residual, err := model.InternalToUnits(base+".oy", patch.Origin.Y)
		if err != nil {
			return nil, nil, err
		}
		if residual != 0 {
			*losses = append(*losses, floorLoss(base+".oy", patch.Origin.Y, oy, "internal logical unit", Version3))
		}
		out.OriginX, out.OriginY = &ox, &oy
	}
	if patch.Resolution != nil {
		w, h := patch.Resolution.W, patch.Resolution.H
		out.Width, out.Height = &w, &h
	}
	if patch.Scale != nil {
		v := v3ScaleOrdinal[*patch.Scale]
		out.Scale = &v
	}
	if patch.Chroma != nil {
		v := v3ChromaOrdinal[*patch.Chroma]
		out.Chroma = &v
	}
	if patch.CapabilityScales != nil {
		values := make([]int64, 0, len(*patch.CapabilityScales))
		for _, scale := range *patch.CapabilityScales {
			values = append(values, v3ScaleOrdinal[scale])
		}
		out.CapabilityScales = &values
	}
	if patch.CapabilityChromas != nil {
		values := make([]int64, 0, len(*patch.CapabilityChromas))
		for _, chroma := range *patch.CapabilityChromas {
			values = append(values, v3ChromaOrdinal[chroma])
		}
		out.CapabilityChromas = &values
	}
	if patch.MaxPacketSize != nil {
		v := *patch.MaxPacketSize
		out.MaxPacketSize = &v
	}
	return out, *losses, nil
}

func encodeV3Region(region model.Rect, base string, losses *[]model.Loss) (v3Region, []model.Loss, error) {
	var out v3Region
	values := []struct {
		key    string
		source int64
		target *int64
	}{
		{"x", region.X, &out.X},
		{"y", region.Y, &out.Y},
		{"w", region.W, &out.W},
		{"h", region.H, &out.H},
	}
	for _, item := range values {
		v, residual, err := model.InternalToUnits(base+"."+item.key, item.source)
		if err != nil {
			return v3Region{}, nil, err
		}
		if residual != 0 {
			*losses = append(*losses, floorLoss(base+"."+item.key, item.source, v, "internal logical unit", Version3))
		}
		*item.target = v
	}
	return out, *losses, nil
}

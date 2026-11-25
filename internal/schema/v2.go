package schema

import (
	"fmt"

	"obsproj/internal/model"
)

type v2Screen struct {
	ScreenID      string       `json:"screenId"`
	DisplayName   string       `json:"displayName"`
	OriginX       int64        `json:"originX"`
	OriginY       int64        `json:"originY"`
	Width         int64        `json:"width"`
	Height        int64        `json:"height"`
	Scale         int64        `json:"scale"`
	Chroma        int64        `json:"chroma"`
	Capability    v2Capability `json:"capability"`
	MaxPacketSize *int         `json:"maxPacketSize,omitempty"`
}

type v2Capability struct {
	Scales  []int64 `json:"scales"`
	Chromas []int64 `json:"chromas"`
}

type v2Topology struct {
	OriginX           *int64   `json:"originX,omitempty"`
	OriginY           *int64   `json:"originY,omitempty"`
	Width             *int64   `json:"width,omitempty"`
	Height            *int64   `json:"height,omitempty"`
	Scale             *int64   `json:"scale,omitempty"`
	Chroma            *int64   `json:"chroma,omitempty"`
	CapabilityScales  *[]int64 `json:"capabilityScales,omitempty"`
	CapabilityChromas *[]int64 `json:"capabilityChromas,omitempty"`
	MaxPacketSize     *int     `json:"maxPacketSize,omitempty"`
}

type v2Event struct {
	EventID   string      `json:"eventId"`
	WindowNo  int         `json:"windowNo"`
	AtTick    int64       `json:"atTick"`
	EventType string      `json:"eventType"`
	ScreenID  string      `json:"screenId"`
	Topology  *v2Topology `json:"topologyPatch,omitempty"`
}

type v2Region struct {
	OffsetX int64 `json:"offsetX"`
	OffsetY int64 `json:"offsetY"`
	W       int64 `json:"w"`
	H       int64 `json:"h"`
}

type v2Frame struct {
	WindowNo int      `json:"windowNo"`
	AtTick   int64    `json:"atTick"`
	ScreenID string   `json:"screenId"`
	Region   v2Region `json:"region"`
}

type v2Session struct {
	SchemaVersion string     `json:"schema_version"`
	SessionID     string     `json:"sessionId"`
	Seed          int64      `json:"seed"`
	TickRate      int64      `json:"tickRate"`
	Screens       []v2Screen `json:"screens"`
	Events        []v2Event  `json:"events"`
	Frames        []v2Frame  `json:"frames"`
}

func decodeV2(root object) (*model.Session, *model.KernelError) {
	const version = Version2
	sessionID, err := stringField(root, "sessionId", version, "session_id", "sessionId")
	if err != nil {
		return nil, err
	}
	seed, err := intField(root, "seed", version, "seed", "seed")
	if err != nil {
		return nil, err
	}
	tickRate, err := intField(root, "tickRate", version, "tick_rate", "tickRate")
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
	origins := map[string]model.Point{}

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
		id, ferr := stringField(obj, "screenId", version, base+".screenId", "screenId")
		if ferr != nil {
			return nil, ferr
		}
		name, ferr := stringField(obj, "displayName", version, base+".displayName", "displayName")
		if ferr != nil {
			return nil, ferr
		}
		ox, ferr := intField(obj, "originX", version, base+".originX", "originX")
		if ferr != nil {
			return nil, ferr
		}
		oy, ferr := intField(obj, "originY", version, base+".originY", "originY")
		if ferr != nil {
			return nil, ferr
		}
		w, ferr := intField(obj, "width", version, base+".width", "width")
		if ferr != nil {
			return nil, ferr
		}
		h, ferr := intField(obj, "height", version, base+".height", "height")
		if ferr != nil {
			return nil, ferr
		}
		originX, ovf := model.UnitsToInternal(base+".originX", ox)
		if ovf != nil {
			ovf.Version = version
			return nil, ovf
		}
		originY, ovf := model.UnitsToInternal(base+".originY", oy)
		if ovf != nil {
			ovf.Version = version
			return nil, ovf
		}
		scaleValue, ferr := intField(obj, "scale", version, base+".scale", "scale")
		if ferr != nil {
			return nil, ferr
		}
		scale, ferr := v2ScaleFromInt(scaleValue, version, base+".scale")
		if ferr != nil {
			return nil, ferr
		}
		chromaValue, ferr := intField(obj, "chroma", version, base+".chroma", "chroma")
		if ferr != nil {
			return nil, ferr
		}
		chroma, ferr := v2ChromaFromInt(chromaValue, version, base+".chroma")
		if ferr != nil {
			return nil, ferr
		}
		capObj, ferr := objectField(obj, "capability", version, base+".capability", "capability")
		if ferr != nil {
			return nil, ferr
		}
		capScales, ferr := decodeV2ScaleList(capObj, "scales", version, base+".capability.scales")
		if ferr != nil {
			return nil, ferr
		}
		capChromas, ferr := decodeV2ChromaList(capObj, "chromas", version, base+".capability.chromas")
		if ferr != nil {
			return nil, ferr
		}
		maxPacket := model.DefaultMaxPacketSize
		if rawMax, ok := obj["maxPacketSize"]; ok {
			value, intErr := parseInt(rawMax, version, base+".maxPacketSize", "maxPacketSize")
			if intErr != nil {
				return nil, intErr
			}
			if value <= 0 || value > int64(^uint(0)>>1) {
				return nil, schemaError(version, base+".maxPacketSize", "maxPacketSize", "invalid packet size")
			}
			maxPacket = int(value)
		} else {
			session.UpgradeTraces = append(session.UpgradeTraces, model.DefaultTrace{
				Path:         "screens." + id + ".maxPacketSize",
				OldValue:     "<missing>",
				DefaultValue: fmt.Sprintf("%d", model.DefaultMaxPacketSize),
				Source:       "schema:v2.defaultMaxPacketSize",
			})
		}
		screen := model.Screen{
			ID:         id,
			Name:       name,
			Origin:     model.Point{X: originX, Y: originY},
			Resolution: model.Size{W: w, H: h},
			Scale:      scale,
			Chroma:     chroma,
			Capability: model.Capability{
				Scales:        capScales,
				Chromas:       capChromas,
				MaxPacketSize: maxPacket,
			},
		}
		session.Screens = append(session.Screens, screen)
		origins[id] = screen.Origin
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
		id, ferr := stringField(obj, "eventId", version, base+".eventId", "eventId")
		if ferr != nil {
			return nil, ferr
		}
		window, ferr := intField(obj, "windowNo", version, base+".windowNo", "windowNo")
		if ferr != nil {
			return nil, ferr
		}
		atTick, ferr := intField(obj, "atTick", version, base+".atTick", "atTick")
		if ferr != nil {
			return nil, ferr
		}
		at, ovf := model.NativeTicksToInternal(base+".atTick", atTick, tickRate, internalRate)
		if ovf != nil {
			ovf.Version = version
			return nil, ovf
		}
		kindText, ferr := stringField(obj, "eventType", version, base+".eventType", "eventType")
		if ferr != nil {
			return nil, ferr
		}
		kind := model.EventKind(kindText)
		if kind != model.EventJoin && kind != model.EventExit && kind != model.EventTopology {
			return nil, schemaError(version, base+".eventType", "eventType", "unknown event kind")
		}
		screenID, ferr := stringField(obj, "screenId", version, base+".screenId", "screenId")
		if ferr != nil {
			return nil, ferr
		}
		var patch *model.TopologyPatch
		topo, present, ferr := optionalObject(obj, "topologyPatch", version, base+".topologyPatch", "topologyPatch")
		if ferr != nil {
			return nil, ferr
		}
		if present || kind == model.EventTopology {
			if !present {
				return nil, schemaError(version, base+".topologyPatch", "topologyPatch", "topology patch is required")
			}
			patch, ferr = decodeV2Topology(topo, version, base+".topologyPatch")
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
	originTimelines := buildOriginTimeline(session)

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
		window, ferr := intField(obj, "windowNo", version, base+".windowNo", "windowNo")
		if ferr != nil {
			return nil, ferr
		}
		atTick, ferr := intField(obj, "atTick", version, base+".atTick", "atTick")
		if ferr != nil {
			return nil, ferr
		}
		at, ovf := model.NativeTicksToInternal(base+".atTick", atTick, tickRate, internalRate)
		if ovf != nil {
			ovf.Version = version
			return nil, ovf
		}
		screenID, ferr := stringField(obj, "screenId", version, base+".screenId", "screenId")
		if ferr != nil {
			return nil, ferr
		}
		origin, ok := origins[screenID]
		if !ok {
			return nil, schemaError(version, base+".screenId", "screenId", "unknown screen")
		}
		origin = originAt(origin, originTimelines[screenID], at)
		regionObj, ferr := objectField(obj, "region", version, base+".region", "region")
		if ferr != nil {
			return nil, ferr
		}
		region, ferr := decodeV2Region(regionObj, origin, version, base+".region")
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

func decodeV2ScaleList(obj object, key, version, field string) ([]model.Scale, *model.KernelError) {
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
		scale, scaleErr := v2ScaleFromInt(value, version, fmt.Sprintf("%s[%d]", field, i))
		if scaleErr != nil {
			return nil, scaleErr
		}
		out = append(out, scale)
	}
	return out, nil
}

func decodeV2ChromaList(obj object, key, version, field string) ([]model.Chroma, *model.KernelError) {
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
		chroma, chromaErr := v2ChromaFromInt(value, version, fmt.Sprintf("%s[%d]", field, i))
		if chromaErr != nil {
			return nil, chromaErr
		}
		out = append(out, chroma)
	}
	return out, nil
}

func decodeV2Topology(obj object, version, base string) (*model.TopologyPatch, *model.KernelError) {
	patch := &model.TopologyPatch{}
	_, hasOriginX := obj["originX"]
	_, hasOriginY := obj["originY"]
	if hasOriginX != hasOriginY {
		return nil, schemaError(version, base+".originX", "originX/originY", "originX and originY must be supplied together")
	}
	_, hasWidth := obj["width"]
	_, hasHeight := obj["height"]
	if hasWidth != hasHeight {
		return nil, schemaError(version, base+".width", "width/height", "width and height must be supplied together")
	}
	if raw, ok := obj["originX"]; ok {
		v, err := parseInt(raw, version, base+".originX", "originX")
		if err != nil {
			return nil, err
		}
		micro, ovf := model.UnitsToInternal(base+".originX", v)
		if ovf != nil {
			ovf.Version = version
			return nil, ovf
		}
		patch.Origin = &model.Point{X: micro}
	}
	if raw, ok := obj["originY"]; ok {
		v, err := parseInt(raw, version, base+".originY", "originY")
		if err != nil {
			return nil, err
		}
		micro, ovf := model.UnitsToInternal(base+".originY", v)
		if ovf != nil {
			ovf.Version = version
			return nil, ovf
		}
		if patch.Origin == nil {
			patch.Origin = &model.Point{}
		}
		patch.Origin.Y = micro
	}
	if raw, ok := obj["width"]; ok {
		v, err := parseInt(raw, version, base+".width", "width")
		if err != nil {
			return nil, err
		}
		if patch.Resolution == nil {
			patch.Resolution = &model.Size{}
		}
		patch.Resolution.W = v
	}
	if raw, ok := obj["height"]; ok {
		v, err := parseInt(raw, version, base+".height", "height")
		if err != nil {
			return nil, err
		}
		if patch.Resolution == nil {
			patch.Resolution = &model.Size{}
		}
		patch.Resolution.H = v
	}
	if raw, ok := obj["scale"]; ok {
		v, err := parseInt(raw, version, base+".scale", "scale")
		if err != nil {
			return nil, err
		}
		scale, scaleErr := v2ScaleFromInt(v, version, base+".scale")
		if scaleErr != nil {
			return nil, scaleErr
		}
		patch.Scale = &scale
	}
	if raw, ok := obj["chroma"]; ok {
		v, err := parseInt(raw, version, base+".chroma", "chroma")
		if err != nil {
			return nil, err
		}
		chroma, chromaErr := v2ChromaFromInt(v, version, base+".chroma")
		if chromaErr != nil {
			return nil, chromaErr
		}
		patch.Chroma = &chroma
	}
	if _, ok := obj["capabilityScales"]; ok {
		scales, err := decodeV2ScaleList(obj, "capabilityScales", version, base+".capabilityScales")
		if err != nil {
			return nil, err
		}
		patch.CapabilityScales = &scales
	}
	if _, ok := obj["capabilityChromas"]; ok {
		chromas, err := decodeV2ChromaList(obj, "capabilityChromas", version, base+".capabilityChromas")
		if err != nil {
			return nil, err
		}
		patch.CapabilityChromas = &chromas
	}
	if raw, ok := obj["maxPacketSize"]; ok {
		v, err := parseInt(raw, version, base+".maxPacketSize", "maxPacketSize")
		if err != nil {
			return nil, err
		}
		if v <= 0 || v > int64(^uint(0)>>1) {
			return nil, schemaError(version, base+".maxPacketSize", "maxPacketSize", "invalid packet size")
		}
		n := int(v)
		patch.MaxPacketSize = &n
	}
	return patch, nil
}

func decodeV2Region(obj object, origin model.Point, version, base string) (model.Rect, *model.KernelError) {
	var out model.Rect
	offsetX, err := intField(obj, "offsetX", version, base+".offsetX", "offsetX")
	if err != nil {
		return out, err
	}
	offsetY, err := intField(obj, "offsetY", version, base+".offsetY", "offsetY")
	if err != nil {
		return out, err
	}
	x, ovf := model.AbsoluteFromOffset(base+".offsetX", origin.X, offsetX)
	if ovf != nil {
		ovf.Version = version
		return out, ovf
	}
	y, ovf := model.AbsoluteFromOffset(base+".offsetY", origin.Y, offsetY)
	if ovf != nil {
		ovf.Version = version
		return out, ovf
	}
	w, err := intField(obj, "w", version, base+".w", "w")
	if err != nil {
		return out, err
	}
	h, err := intField(obj, "h", version, base+".h", "h")
	if err != nil {
		return out, err
	}
	width, ovf := model.UnitsToInternal(base+".w", w)
	if ovf != nil {
		ovf.Version = version
		return out, ovf
	}
	height, ovf := model.UnitsToInternal(base+".h", h)
	if ovf != nil {
		ovf.Version = version
		return out, ovf
	}
	out = model.Rect{X: x, Y: y, W: width, H: height}
	return out, nil
}

func encodeV2(session *model.Session) ([]byte, []model.Loss, error) {
	var losses []model.Loss
	originTimelines := buildOriginTimeline(session)
	out := v2Session{
		SchemaVersion: Version2,
		SessionID:     session.ID,
		Seed:          session.Seed,
		TickRate:      session.TickRate,
	}
	for i := range session.Screens {
		screen := &session.Screens[i]
		base := fmt.Sprintf("screens[%d]", i)
		ox, residual, err := model.InternalToUnits(base+".originX", screen.Origin.X)
		if err != nil {
			return nil, nil, err
		}
		if residual != 0 {
			losses = append(losses, floorLoss(base+".originX", screen.Origin.X, ox, "internal logical unit", Version2))
		}
		oy, residual, err := model.InternalToUnits(base+".originY", screen.Origin.Y)
		if err != nil {
			return nil, nil, err
		}
		if residual != 0 {
			losses = append(losses, floorLoss(base+".originY", screen.Origin.Y, oy, "internal logical unit", Version2))
		}
		capScales := make([]int64, 0, len(screen.Capability.Scales))
		for _, scale := range screen.Capability.Scales {
			capScales = append(capScales, int64(scale))
		}
		capChromas := make([]int64, 0, len(screen.Capability.Chromas))
		for _, chroma := range screen.Capability.Chromas {
			capChromas = append(capChromas, v2IntChromas[chroma])
		}
		maxPacket := screen.Capability.MaxPacketSize
		out.Screens = append(out.Screens, v2Screen{
			ScreenID:      screen.ID,
			DisplayName:   screen.Name,
			OriginX:       ox,
			OriginY:       oy,
			Width:         screen.Resolution.W,
			Height:        screen.Resolution.H,
			Scale:         int64(screen.Scale),
			Chroma:        v2IntChromas[screen.Chroma],
			Capability:    v2Capability{Scales: capScales, Chromas: capChromas},
			MaxPacketSize: &maxPacket,
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
			losses = append(losses, floorLoss(base+".atTick", event.At, ticks, "native tick", Version2))
		}
		native := v2Event{
			EventID:   event.ID,
			WindowNo:  event.Window,
			AtTick:    ticks,
			EventType: string(event.Kind),
			ScreenID:  event.ScreenID,
		}
		if event.Topology != nil {
			topo, topoLosses, encErr := encodeV2Topology(event.Topology, base+".topologyPatch", &losses)
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
			losses = append(losses, floorLoss(base+".atTick", command.At, ticks, "native tick", Version2))
		}
		var screen *model.Screen
		for j := range session.Screens {
			if session.Screens[j].ID == command.ScreenID {
				screen = &session.Screens[j]
				break
			}
		}
		if screen == nil {
			return nil, nil, model.NewError(model.CodeSchema, "frame references unknown screen")
		}
		origin := originAt(screen.Origin, originTimelines[command.ScreenID], command.At)
		region, regionLosses, encErr := encodeV2Region(command.Region, origin, base+".region", &losses)
		if encErr != nil {
			return nil, nil, encErr
		}
		losses = regionLosses
		out.Frames = append(out.Frames, v2Frame{
			WindowNo: command.Window,
			AtTick:   ticks,
			ScreenID: command.ScreenID,
			Region:   region,
		})
	}
	data, err := marshal(out)
	return data, losses, err
}

func encodeV2Topology(patch *model.TopologyPatch, base string, losses *[]model.Loss) (*v2Topology, []model.Loss, error) {
	out := &v2Topology{}
	if patch.Origin != nil {
		ox, residual, err := model.InternalToUnits(base+".originX", patch.Origin.X)
		if err != nil {
			return nil, nil, err
		}
		if residual != 0 {
			*losses = append(*losses, floorLoss(base+".originX", patch.Origin.X, ox, "internal logical unit", Version2))
		}
		oy, residual, err := model.InternalToUnits(base+".originY", patch.Origin.Y)
		if err != nil {
			return nil, nil, err
		}
		if residual != 0 {
			*losses = append(*losses, floorLoss(base+".originY", patch.Origin.Y, oy, "internal logical unit", Version2))
		}
		out.OriginX, out.OriginY = &ox, &oy
	}
	if patch.Resolution != nil {
		w, h := patch.Resolution.W, patch.Resolution.H
		out.Width, out.Height = &w, &h
	}
	if patch.Scale != nil {
		v := int64(*patch.Scale)
		out.Scale = &v
	}
	if patch.Chroma != nil {
		v := v2IntChromas[*patch.Chroma]
		out.Chroma = &v
	}
	if patch.CapabilityScales != nil {
		values := make([]int64, 0, len(*patch.CapabilityScales))
		for _, scale := range *patch.CapabilityScales {
			values = append(values, int64(scale))
		}
		out.CapabilityScales = &values
	}
	if patch.CapabilityChromas != nil {
		values := make([]int64, 0, len(*patch.CapabilityChromas))
		for _, chroma := range *patch.CapabilityChromas {
			values = append(values, v2IntChromas[chroma])
		}
		out.CapabilityChromas = &values
	}
	if patch.MaxPacketSize != nil {
		v := *patch.MaxPacketSize
		out.MaxPacketSize = &v
	}
	return out, *losses, nil
}

func encodeV2Region(region model.Rect, origin model.Point, base string, losses *[]model.Loss) (v2Region, []model.Loss, error) {
	var out v2Region
	x, residual, err := model.OffsetFromAbsolute(base+".offsetX", region.X, origin.X)
	if err != nil {
		return out, nil, err
	}
	if residual != 0 {
		*losses = append(*losses, floorLoss(base+".offsetX", region.X-origin.X, x, "internal logical unit", Version2))
	}
	y, residual, err := model.OffsetFromAbsolute(base+".offsetY", region.Y, origin.Y)
	if err != nil {
		return out, nil, err
	}
	if residual != 0 {
		*losses = append(*losses, floorLoss(base+".offsetY", region.Y-origin.Y, y, "internal logical unit", Version2))
	}
	w, residual, err := model.InternalToUnits(base+".w", region.W)
	if err != nil {
		return out, nil, err
	}
	if residual != 0 {
		*losses = append(*losses, floorLoss(base+".w", region.W, w, "internal logical unit", Version2))
	}
	h, residual, err := model.InternalToUnits(base+".h", region.H)
	if err != nil {
		return out, nil, err
	}
	if residual != 0 {
		*losses = append(*losses, floorLoss(base+".h", region.H, h, "internal logical unit", Version2))
	}
	out = v2Region{OffsetX: x, OffsetY: y, W: w, H: h}
	return out, *losses, nil
}

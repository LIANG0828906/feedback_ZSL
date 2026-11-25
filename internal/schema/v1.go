package schema

import (
	"encoding/json"
	"fmt"

	"obsproj/internal/model"
)

type v1Screen struct {
	ID         string       `json:"id"`
	Name       string       `json:"name"`
	OriginX    int64        `json:"origin_x"`
	OriginY    int64        `json:"origin_y"`
	Width      int64        `json:"width"`
	Height     int64        `json:"height"`
	Scale      string       `json:"scale"`
	Chroma     string       `json:"chroma"`
	Capability v1Capability `json:"capability"`
}

type v1Capability struct {
	Scales  []string `json:"scales"`
	Chromas []string `json:"chromas"`
}

type v1Topology struct {
	OriginX *int64  `json:"origin_x,omitempty"`
	OriginY *int64  `json:"origin_y,omitempty"`
	Width   *int64  `json:"width,omitempty"`
	Height  *int64  `json:"height,omitempty"`
	Scale   *string `json:"scale,omitempty"`
	Chroma  *string `json:"chroma,omitempty"`
}

type v1Event struct {
	EventID  string      `json:"event_id"`
	WindowNo int         `json:"window_no"`
	AtMS     int64       `json:"at_ms"`
	Kind     string      `json:"kind"`
	ScreenID string      `json:"screen_id"`
	Topology *v1Topology `json:"topology,omitempty"`
}

type v1Region struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
	W int64 `json:"w"`
	H int64 `json:"h"`
}

type v1Frame struct {
	WindowNo int      `json:"window_no"`
	AtMS     int64    `json:"at_ms"`
	ScreenID string   `json:"screen_id"`
	Region   v1Region `json:"region"`
}

type v1Session struct {
	SchemaVersion string     `json:"schema_version"`
	SessionID     string     `json:"session_id"`
	Seed          int64      `json:"seed"`
	TickRate      int64      `json:"tick_rate"`
	Screens       []v1Screen `json:"screens"`
	Events        []v1Event  `json:"events"`
	Frames        []v1Frame  `json:"frames"`
}

func decodeV1(root object) (*model.Session, *model.KernelError) {
	const version = Version1
	sessionID, err := stringField(root, "session_id", version, "session_id", "session_id")
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
		id, ferr := stringField(obj, "id", version, base+".id", "id")
		if ferr != nil {
			return nil, ferr
		}
		name, ferr := stringField(obj, "name", version, base+".name", "name")
		if ferr != nil {
			return nil, ferr
		}
		ox, ferr := intField(obj, "origin_x", version, base+".origin_x", "origin_x")
		if ferr != nil {
			return nil, ferr
		}
		oy, ferr := intField(obj, "origin_y", version, base+".origin_y", "origin_y")
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
		originX, ovf := model.UnitsToInternal(base+".origin_x", ox)
		if ovf != nil {
			ovf.Version = version
			return nil, ovf
		}
		originY, ovf := model.UnitsToInternal(base+".origin_y", oy)
		if ovf != nil {
			ovf.Version = version
			return nil, ovf
		}
		scaleText, ferr := stringField(obj, "scale", version, base+".scale", "scale")
		if ferr != nil {
			return nil, ferr
		}
		scale, ok := v1ScaleStrings[scaleText]
		if !ok {
			return nil, schemaError(version, base+".scale", "scale", "unknown scale encoding")
		}
		chromaText, ferr := stringField(obj, "chroma", version, base+".chroma", "chroma")
		if ferr != nil {
			return nil, ferr
		}
		chroma, ok := v1ChromaStrings[chromaText]
		if !ok {
			return nil, schemaError(version, base+".chroma", "chroma", "unknown chroma encoding")
		}
		capObj, ferr := objectField(obj, "capability", version, base+".capability", "capability")
		if ferr != nil {
			return nil, ferr
		}
		capScales, ferr := decodeV1ScaleList(capObj, "scales", version, base+".capability.scales")
		if ferr != nil {
			return nil, ferr
		}
		capChromas, ferr := decodeV1ChromaList(capObj, "chromas", version, base+".capability.chromas")
		if ferr != nil {
			return nil, ferr
		}
		session.Screens = append(session.Screens, model.Screen{
			ID:         id,
			Name:       name,
			Origin:     model.Point{X: originX, Y: originY},
			Resolution: model.Size{W: w, H: h},
			Scale:      scale,
			Chroma:     chroma,
			Capability: model.Capability{
				Scales:        capScales,
				Chromas:       capChromas,
				MaxPacketSize: model.DefaultMaxPacketSize,
			},
		})
		session.UpgradeTraces = append(session.UpgradeTraces, model.DefaultTrace{
			Path:         "screens." + id + ".capability.max_packet_size",
			OldValue:     "<missing>",
			DefaultValue: fmt.Sprintf("%d", model.DefaultMaxPacketSize),
			Source:       "schema:v1.defaultMaxPacketSize",
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
		id, ferr := stringField(obj, "event_id", version, base+".event_id", "event_id")
		if ferr != nil {
			return nil, ferr
		}
		window, ferr := intField(obj, "window_no", version, base+".window_no", "window_no")
		if ferr != nil {
			return nil, ferr
		}
		atMS, ferr := intField(obj, "at_ms", version, base+".at_ms", "at_ms")
		if ferr != nil {
			return nil, ferr
		}
		at, ovf := model.MillisToInternal(base+".at_ms", atMS, internalRate)
		if ovf != nil {
			ovf.Version = version
			return nil, ovf
		}
		kindText, ferr := stringField(obj, "kind", version, base+".kind", "kind")
		if ferr != nil {
			return nil, ferr
		}
		kind := model.EventKind(kindText)
		if kind != model.EventJoin && kind != model.EventExit && kind != model.EventTopology {
			return nil, schemaError(version, base+".kind", "kind", "unknown event kind")
		}
		screenID, ferr := stringField(obj, "screen_id", version, base+".screen_id", "screen_id")
		if ferr != nil {
			return nil, ferr
		}
		var patch *model.TopologyPatch
		topo, present, ferr := optionalObject(obj, "topology", version, base+".topology", "topology")
		if ferr != nil {
			return nil, ferr
		}
		if present || kind == model.EventTopology {
			if !present {
				return nil, schemaError(version, base+".topology", "topology", "topology patch is required")
			}
			patch, ferr = decodeV1Topology(topo, version, base+".topology")
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
		window, ferr := intField(obj, "window_no", version, base+".window_no", "window_no")
		if ferr != nil {
			return nil, ferr
		}
		atMS, ferr := intField(obj, "at_ms", version, base+".at_ms", "at_ms")
		if ferr != nil {
			return nil, ferr
		}
		at, ovf := model.MillisToInternal(base+".at_ms", atMS, internalRate)
		if ovf != nil {
			ovf.Version = version
			return nil, ovf
		}
		screenID, ferr := stringField(obj, "screen_id", version, base+".screen_id", "screen_id")
		if ferr != nil {
			return nil, ferr
		}
		regionObj, ferr := objectField(obj, "region", version, base+".region", "region")
		if ferr != nil {
			return nil, ferr
		}
		region, ferr := decodeV1Region(regionObj, version, base+".region")
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

func decodeV1ScaleList(obj object, key, version, field string) ([]model.Scale, *model.KernelError) {
	raws, err := arrayField(obj, key, version, field, key)
	if err != nil {
		return nil, err
	}
	out := make([]model.Scale, 0, len(raws))
	for i, raw := range raws {
		var text string
		if jsonErr := json.Unmarshal(raw, &text); jsonErr != nil {
			return nil, schemaError(version, fmt.Sprintf("%s[%d]", field, i), key, "expected string")
		}
		scale, ok := v1ScaleStrings[text]
		if !ok {
			return nil, schemaError(version, fmt.Sprintf("%s[%d]", field, i), key, "unknown scale encoding")
		}
		out = append(out, scale)
	}
	return out, nil
}

func decodeV1ChromaList(obj object, key, version, field string) ([]model.Chroma, *model.KernelError) {
	raws, err := arrayField(obj, key, version, field, key)
	if err != nil {
		return nil, err
	}
	out := make([]model.Chroma, 0, len(raws))
	for i, raw := range raws {
		var text string
		if jsonErr := json.Unmarshal(raw, &text); jsonErr != nil {
			return nil, schemaError(version, fmt.Sprintf("%s[%d]", field, i), key, "expected string")
		}
		chroma, ok := v1ChromaStrings[text]
		if !ok {
			return nil, schemaError(version, fmt.Sprintf("%s[%d]", field, i), key, "unknown chroma encoding")
		}
		out = append(out, chroma)
	}
	return out, nil
}

func decodeV1Topology(obj object, version, base string) (*model.TopologyPatch, *model.KernelError) {
	patch := &model.TopologyPatch{}
	_, hasOriginX := obj["origin_x"]
	_, hasOriginY := obj["origin_y"]
	if hasOriginX != hasOriginY {
		return nil, schemaError(version, base+".origin_x", "origin_x/origin_y", "origin_x and origin_y must be supplied together")
	}
	_, hasWidth := obj["width"]
	_, hasHeight := obj["height"]
	if hasWidth != hasHeight {
		return nil, schemaError(version, base+".width", "width/height", "width and height must be supplied together")
	}
	if raw, ok := obj["origin_x"]; ok {
		v, err := parseInt(raw, version, base+".origin_x", "origin_x")
		if err != nil {
			return nil, err
		}
		micro, ovf := model.UnitsToInternal(base+".origin_x", v)
		if ovf != nil {
			ovf.Version = version
			return nil, ovf
		}
		patch.Origin = &model.Point{X: micro}
	}
	if raw, ok := obj["origin_y"]; ok {
		v, err := parseInt(raw, version, base+".origin_y", "origin_y")
		if err != nil {
			return nil, err
		}
		micro, ovf := model.UnitsToInternal(base+".origin_y", v)
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
		var text string
		if jsonErr := json.Unmarshal(raw, &text); jsonErr != nil {
			return nil, schemaError(version, base+".scale", "scale", "expected string")
		}
		scale, exists := v1ScaleStrings[text]
		if !exists {
			return nil, schemaError(version, base+".scale", "scale", "unknown scale encoding")
		}
		patch.Scale = &scale
	}
	if raw, ok := obj["chroma"]; ok {
		var text string
		if jsonErr := json.Unmarshal(raw, &text); jsonErr != nil {
			return nil, schemaError(version, base+".chroma", "chroma", "expected string")
		}
		chroma, exists := v1ChromaStrings[text]
		if !exists {
			return nil, schemaError(version, base+".chroma", "chroma", "unknown chroma encoding")
		}
		patch.Chroma = &chroma
	}
	return patch, nil
}

func decodeV1Region(obj object, version, base string) (model.Rect, *model.KernelError) {
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

func encodeV1(session *model.Session) ([]byte, []model.Loss, error) {
	var losses []model.Loss
	out := v1Session{
		SchemaVersion: Version1,
		SessionID:     session.ID,
		Seed:          session.Seed,
		TickRate:      session.TickRate,
	}
	for i := range session.Screens {
		screen := &session.Screens[i]
		base := fmt.Sprintf("screens[%d]", i)
		degradeV1Screen(screen, base, &losses)
		ox, residual, ovf := model.InternalToUnits(base+".origin_x", screen.Origin.X)
		if ovf != nil {
			return nil, nil, ovf
		}
		if residual != 0 {
			losses = append(losses, floorLoss(base+".origin_x", screen.Origin.X, ox, "internal logical unit", Version1))
		}
		oy, residual, ovf := model.InternalToUnits(base+".origin_y", screen.Origin.Y)
		if ovf != nil {
			return nil, nil, ovf
		}
		if residual != 0 {
			losses = append(losses, floorLoss(base+".origin_y", screen.Origin.Y, oy, "internal logical unit", Version1))
		}
		scaleText := v1StringScales[screen.Scale]
		chromaText := v1StringChromas[screen.Chroma]
		capScales := make([]string, 0, len(screen.Capability.Scales))
		for _, scale := range screen.Capability.Scales {
			capScales = append(capScales, v1StringScales[scale])
		}
		capChromas := make([]string, 0, len(screen.Capability.Chromas))
		for _, chroma := range screen.Capability.Chromas {
			capChromas = append(capChromas, v1StringChromas[chroma])
		}
		out.Screens = append(out.Screens, v1Screen{
			ID:         screen.ID,
			Name:       screen.Name,
			OriginX:    ox,
			OriginY:    oy,
			Width:      screen.Resolution.W,
			Height:     screen.Resolution.H,
			Scale:      scaleText,
			Chroma:     chromaText,
			Capability: v1Capability{Scales: capScales, Chromas: capChromas},
		})
	}
	for i := range session.Events {
		event := &session.Events[i]
		base := fmt.Sprintf("events[%d]", i)
		atMS, residual, ovf := model.InternalToMillis(event.At, session.InternalRate)
		if ovf != nil {
			return nil, nil, ovf
		}
		if residual != 0 {
			losses = append(losses, floorLoss(base+".at_ms", event.At, atMS, "millisecond", Version1))
		}
		native := v1Event{
			EventID:  event.ID,
			WindowNo: event.Window,
			AtMS:     atMS,
			Kind:     string(event.Kind),
			ScreenID: event.ScreenID,
		}
		if event.Topology != nil {
			topo, topoLosses, encErr := encodeV1Topology(event.Topology, base+".topology", &losses)
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
		atMS, residual, ovf := model.InternalToMillis(command.At, session.InternalRate)
		if ovf != nil {
			return nil, nil, ovf
		}
		if residual != 0 {
			losses = append(losses, floorLoss(base+".at_ms", command.At, atMS, "millisecond", Version1))
		}
		region, regionLosses, encErr := encodeV1Region(command.Region, base+".region", &losses)
		if encErr != nil {
			return nil, nil, encErr
		}
		losses = regionLosses
		out.Frames = append(out.Frames, v1Frame{
			WindowNo: command.Window,
			AtMS:     atMS,
			ScreenID: command.ScreenID,
			Region:   region,
		})
	}
	data, err := marshal(out)
	return data, losses, err
}

func degradeV1Screen(screen *model.Screen, base string, losses *[]model.Loss) {
	if !v1SupportedScales[screen.Scale] {
		replacement := nearestV1Scale(screen.Scale)
		*losses = append(*losses, model.Loss{
			Path:        base + ".scale",
			OldValue:    screen.Scale.String(),
			NewValue:    replacement.String(),
			Disposition: "nearest supported lower scale",
			Version:     Version1,
		})
		screen.Scale = replacement
	}
	if !v1SupportedChromas[screen.Chroma] {
		replacement := model.Chroma422
		*losses = append(*losses, model.Loss{
			Path:        base + ".chroma",
			OldValue:    screen.Chroma.String(),
			NewValue:    replacement.String(),
			Disposition: "downgrade to 4:2:2",
			Version:     Version1,
		})
		screen.Chroma = replacement
	}
	filteredScales := make([]model.Scale, 0, len(screen.Capability.Scales))
	for _, scale := range screen.Capability.Scales {
		if v1SupportedScales[scale] {
			filteredScales = append(filteredScales, scale)
			continue
		}
		*losses = append(*losses, model.Loss{
			Path:        base + ".capability.scales",
			OldValue:    scale.String(),
			NewValue:    "<removed>",
			Disposition: "removed unsupported capability entry",
			Version:     Version1,
		})
	}
	if !containsScale(filteredScales, screen.Scale) {
		filteredScales = append(filteredScales, screen.Scale)
	}
	screen.Capability.Scales = filteredScales

	filteredChromas := make([]model.Chroma, 0, len(screen.Capability.Chromas))
	for _, chroma := range screen.Capability.Chromas {
		if v1SupportedChromas[chroma] {
			filteredChromas = append(filteredChromas, chroma)
			continue
		}
		*losses = append(*losses, model.Loss{
			Path:        base + ".capability.chromas",
			OldValue:    chroma.String(),
			NewValue:    "<removed>",
			Disposition: "removed unsupported capability entry",
			Version:     Version1,
		})
	}
	if !containsChroma(filteredChromas, screen.Chroma) {
		filteredChromas = append(filteredChromas, screen.Chroma)
	}
	screen.Capability.Chromas = filteredChromas
	if screen.Capability.MaxPacketSize != model.DefaultMaxPacketSize {
		*losses = append(*losses, model.Loss{
			Path:        base + ".capability.max_packet_size",
			OldValue:    fmt.Sprintf("%d", screen.Capability.MaxPacketSize),
			NewValue:    fmt.Sprintf("%d", model.DefaultMaxPacketSize),
			Disposition: "v1 has no field; loader applies declared default",
			Version:     Version1,
		})
	}
}

func encodeV1Topology(patch *model.TopologyPatch, base string, losses *[]model.Loss) (*v1Topology, []model.Loss, error) {
	out := &v1Topology{}
	if patch.Origin != nil {
		ox, residual, err := model.InternalToUnits(base+".origin_x", patch.Origin.X)
		if err != nil {
			return nil, nil, err
		}
		if residual != 0 {
			*losses = append(*losses, floorLoss(base+".origin_x", patch.Origin.X, ox, "internal logical unit", Version1))
		}
		oy, residual, err := model.InternalToUnits(base+".origin_y", patch.Origin.Y)
		if err != nil {
			return nil, nil, err
		}
		if residual != 0 {
			*losses = append(*losses, floorLoss(base+".origin_y", patch.Origin.Y, oy, "internal logical unit", Version1))
		}
		out.OriginX, out.OriginY = &ox, &oy
	}
	if patch.Resolution != nil {
		w, h := patch.Resolution.W, patch.Resolution.H
		out.Width, out.Height = &w, &h
	}
	if patch.Scale != nil {
		scale := *patch.Scale
		if !v1SupportedScales[scale] {
			replacement := nearestV1Scale(scale)
			*losses = append(*losses, model.Loss{
				Path:        base + ".scale",
				OldValue:    scale.String(),
				NewValue:    replacement.String(),
				Disposition: "nearest supported lower scale",
				Version:     Version1,
			})
			scale = replacement
		}
		text := v1StringScales[scale]
		out.Scale = &text
	}
	if patch.Chroma != nil {
		chroma := *patch.Chroma
		if !v1SupportedChromas[chroma] {
			*losses = append(*losses, model.Loss{
				Path:        base + ".chroma",
				OldValue:    chroma.String(),
				NewValue:    model.Chroma422.String(),
				Disposition: "downgrade to 4:2:2",
				Version:     Version1,
			})
			chroma = model.Chroma422
		}
		text := v1StringChromas[chroma]
		out.Chroma = &text
	}
	if patch.CapabilityScales != nil || patch.CapabilityChromas != nil || patch.MaxPacketSize != nil {
		*losses = append(*losses, model.Loss{
			Path:        base,
			OldValue:    "extended topology patch",
			NewValue:    "origin/resolution/scale/chroma only",
			Disposition: "unsupported patch members omitted",
			Version:     Version1,
		})
	}
	return out, *losses, nil
}

func encodeV1Region(region model.Rect, base string, losses *[]model.Loss) (v1Region, []model.Loss, error) {
	var out v1Region
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
			return v1Region{}, nil, err
		}
		if residual != 0 {
			*losses = append(*losses, floorLoss(base+"."+item.key, item.source, v, "internal logical unit", Version1))
		}
		*item.target = v
	}
	return out, *losses, nil
}

func nearestV1Scale(scale model.Scale) model.Scale {
	choices := []model.Scale{model.Scale100, model.Scale150, model.Scale200}
	best := choices[0]
	for _, candidate := range choices {
		if candidate <= scale {
			best = candidate
		}
	}
	return best
}

func containsScale(values []model.Scale, needle model.Scale) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func containsChroma(values []model.Chroma, needle model.Chroma) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func floorLoss(path string, oldValue, newValue int64, unit, version string) model.Loss {
	return model.Loss{
		Path:        path,
		OldValue:    fmt.Sprintf("%d", oldValue),
		NewValue:    fmt.Sprintf("%d", newValue),
		Disposition: "floor toward negative infinity to " + unit,
		Version:     version,
	}
}

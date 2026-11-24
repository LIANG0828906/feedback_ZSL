package formats

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"virtualstudio/compose"
	"virtualstudio/model"
)

var SupportedVersions = []string{"v1", "v2", "v3"}

type loaderDoc struct {
	FormatVersion string `json:"format_version"`
	Schema        string `json:"schema"`
	Spec          string `json:"spec"`
}

func LoadScene(path string) (*model.Scene, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, model.WrapError(model.CodeIO, path, "read scene", err)
	}
	scene, err := ParseScene(data)
	if err != nil {
		return nil, err
	}
	scene.SourcePath = path
	return scene, nil
}

func ParseScene(data []byte) (*model.Scene, error) {
	var probe loaderDoc
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, model.WrapError(model.CodeDecode, "scene", "invalid JSON", err)
	}
	switch {
	case probe.FormatVersion == "foreground-v1":
		return parseV1(data)
	case probe.Schema == "foreground/v2":
		return parseV2(data)
	case probe.Spec == "virtualstudio/foreground@3":
		return parseV3(data)
	case probe.FormatVersion == "" && probe.Schema == "" && probe.Spec == "":
		return nil, model.NewError(model.CodeVersion, "format_version", "missing version discriminator")
	default:
		version := firstNonEmpty(probe.FormatVersion, probe.Schema, probe.Spec)
		path := "format_version"
		if probe.Schema != "" {
			path = "schema"
		}
		if probe.Spec != "" {
			path = "spec"
		}
		return nil, model.NewError(model.CodeVersion, path, fmt.Sprintf("unsupported source version %q", version))
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

type v1Doc struct {
	FormatVersion   *string   `json:"format_version"`
	Resolution      *v1Size   `json:"resolution"`
	FrameCount      *int      `json:"frame_count"`
	FrameDurationUS *int64    `json:"frame_duration_us"`
	Layers          []v1Layer `json:"layers"`
}

type v1Size struct {
	Width    *int `json:"w"`
	Height   *int `json:"h"`
	Channels *int `json:"channels"`
}

type v1Layer struct {
	ID        *string  `json:"id"`
	Label     string   `json:"label"`
	Visible   *bool    `json:"visible"`
	ZIndex    *int     `json:"z_index"`
	Opacity   *float64 `json:"opacity"`
	AlphaMode *string  `json:"alpha_mode"`
	RGBA      *string  `json:"rgba_base64"`
}

func parseV1(data []byte) (*model.Scene, error) {
	var doc v1Doc
	dec := json.NewDecoder(strings.NewReader(string(data)))
	if err := dec.Decode(&doc); err != nil {
		return nil, model.WrapError(model.CodeDecode, "scene", "invalid v1 document", err)
	}
	if doc.FormatVersion == nil || *doc.FormatVersion == "" {
		return nil, model.NewError(model.CodeVersion, "format_version", "required field is missing")
	}
	if doc.Resolution == nil {
		return nil, model.NewError(model.CodeVersion, "resolution", "required field is missing")
	}
	if doc.Resolution.Width == nil {
		return nil, model.NewError(model.CodeVersion, "resolution.w", "required field is missing")
	}
	if doc.Resolution.Height == nil {
		return nil, model.NewError(model.CodeVersion, "resolution.h", "required field is missing")
	}
	if doc.Resolution.Channels == nil {
		return nil, model.NewError(model.CodeVersion, "resolution.channels", "required field is missing")
	}
	if doc.FrameCount == nil {
		return nil, model.NewError(model.CodeVersion, "frame_count", "required field is missing")
	}
	if doc.FrameDurationUS == nil {
		return nil, model.NewError(model.CodeVersion, "frame_duration_us", "required field is missing")
	}
	if *doc.FrameDurationUS <= 0 || *doc.FrameDurationUS > (1<<63-1)/1_000 {
		return nil, model.NewError(model.CodeParam, "frame_duration_us", "must be positive and safely representable in nanoseconds")
	}
	if len(doc.Layers) == 0 {
		return nil, model.NewError(model.CodeVersion, "layers", "at least one layer is required")
	}
	type ordered struct {
		z    int
		idx  int
		item v1Layer
	}
	items := make([]ordered, 0, len(doc.Layers))
	for i, item := range doc.Layers {
		path := fmt.Sprintf("layers[%d]", i)
		if item.ID == nil || *item.ID == "" {
			return nil, model.NewError(model.CodeVersion, path+".id", "required field is missing")
		}
		if item.Visible == nil {
			return nil, model.NewError(model.CodeVersion, path+".visible", "required field is missing")
		}
		if item.ZIndex == nil {
			return nil, model.NewError(model.CodeVersion, path+".z_index", "required field is missing")
		}
		if item.Opacity == nil {
			return nil, model.NewError(model.CodeVersion, path+".opacity", "required field is missing")
		}
		if item.AlphaMode == nil {
			return nil, model.NewError(model.CodeVersion, path+".alpha_mode", "required field is missing")
		}
		if item.RGBA == nil {
			return nil, model.NewError(model.CodeVersion, path+".rgba_base64", "required field is missing")
		}
		if _, err := parseAlpha(*item.AlphaMode); err != nil {
			return nil, model.NewError(model.CodeVersion, path+".alpha_mode", err.Error())
		}
		if _, err := decodePixels(*item.RGBA, path+".rgba_base64"); err != nil {
			return nil, err
		}
		items = append(items, ordered{z: *item.ZIndex, idx: i, item: item})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].z == items[j].z {
			return items[i].idx < items[j].idx
		}
		return items[i].z < items[j].z
	})
	scene := &model.Scene{
		SourceVersion:   "v1",
		Width:           *doc.Resolution.Width,
		Height:          *doc.Resolution.Height,
		Channels:        *doc.Resolution.Channels,
		FrameCount:      *doc.FrameCount,
		FrameDurationNS: *doc.FrameDurationUS * 1_000,
		OrderExpr:       "z-index",
	}
	for _, entry := range items {
		item := entry.item
		alpha, _ := parseAlpha(*item.AlphaMode)
		raw, err := decodePixels(*item.RGBA, fmt.Sprintf("layers[%d].rgba_base64", entry.idx))
		if err != nil {
			return nil, err
		}
		scene.Layers = append(scene.Layers, model.Layer{
			ID:              *item.ID,
			Name:            item.Label,
			Visible:         *item.Visible,
			Opacity:         *item.Opacity,
			SourceAlphaMode: alpha,
			Pixels:          compose.NormalizePixels(raw, alpha),
		})
	}
	if err := model.ValidateScene(scene); err != nil {
		return nil, err
	}
	return scene, nil
}

type v2Doc struct {
	Schema *string   `json:"schema"`
	Canvas *v2Canvas `json:"canvas"`
	Timing *v2Timing `json:"timing"`
	Stack  []v2Layer `json:"stack"`
}

type v2Canvas struct {
	Width    *int `json:"width"`
	Height   *int `json:"height"`
	Channels *int `json:"channels"`
}

type v2Timing struct {
	FrameCount      *int   `json:"frame_count"`
	FrameDurationMS *int64 `json:"frame_duration_ms"`
}

type v2Layer struct {
	LayerID       *string          `json:"layer_id"`
	DisplayName   string           `json:"display_name"`
	Visibility    *string          `json:"visibility"`
	Opacity       *float64         `json:"opacity"`
	PixelEncoding *v2PixelEncoding `json:"pixel_encoding"`
	RGBA          *string          `json:"rgba"`
}

type v2PixelEncoding struct {
	Alpha *string `json:"alpha"`
}

func parseV2(data []byte) (*model.Scene, error) {
	var doc v2Doc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, model.WrapError(model.CodeDecode, "scene", "invalid v2 document", err)
	}
	if doc.Schema == nil || *doc.Schema == "" {
		return nil, model.NewError(model.CodeVersion, "schema", "required field is missing")
	}
	if doc.Canvas == nil {
		return nil, model.NewError(model.CodeVersion, "canvas", "required field is missing")
	}
	if doc.Canvas.Width == nil {
		return nil, model.NewError(model.CodeVersion, "canvas.width", "required field is missing")
	}
	if doc.Canvas.Height == nil {
		return nil, model.NewError(model.CodeVersion, "canvas.height", "required field is missing")
	}
	if doc.Canvas.Channels == nil {
		return nil, model.NewError(model.CodeVersion, "canvas.channels", "required field is missing")
	}
	if doc.Timing == nil {
		return nil, model.NewError(model.CodeVersion, "timing", "required field is missing")
	}
	if doc.Timing.FrameCount == nil {
		return nil, model.NewError(model.CodeVersion, "timing.frame_count", "required field is missing")
	}
	if doc.Timing.FrameDurationMS == nil {
		return nil, model.NewError(model.CodeVersion, "timing.frame_duration_ms", "required field is missing")
	}
	if *doc.Timing.FrameDurationMS <= 0 || *doc.Timing.FrameDurationMS > (1<<63-1)/1_000_000 {
		return nil, model.NewError(model.CodeParam, "timing.frame_duration_ms", "must be positive and safely representable in nanoseconds")
	}
	if len(doc.Stack) == 0 {
		return nil, model.NewError(model.CodeVersion, "stack", "at least one layer is required")
	}
	scene := &model.Scene{
		SourceVersion:   "v2",
		Width:           *doc.Canvas.Width,
		Height:          *doc.Canvas.Height,
		Channels:        *doc.Canvas.Channels,
		FrameCount:      *doc.Timing.FrameCount,
		FrameDurationNS: *doc.Timing.FrameDurationMS * 1_000_000,
		OrderExpr:       "ordered-stack",
	}
	for i, item := range doc.Stack {
		path := fmt.Sprintf("stack[%d]", i)
		if item.LayerID == nil || *item.LayerID == "" {
			return nil, model.NewError(model.CodeVersion, path+".layer_id", "required field is missing")
		}
		if item.Visibility == nil {
			return nil, model.NewError(model.CodeVersion, path+".visibility", "required field is missing")
		}
		if item.Opacity == nil {
			return nil, model.NewError(model.CodeVersion, path+".opacity", "required field is missing")
		}
		if item.PixelEncoding == nil || item.PixelEncoding.Alpha == nil {
			return nil, model.NewError(model.CodeVersion, path+".pixel_encoding.alpha", "required field is missing")
		}
		if item.RGBA == nil {
			return nil, model.NewError(model.CodeVersion, path+".rgba", "required field is missing")
		}
		visible, err := parseV2Visibility(*item.Visibility)
		if err != nil {
			return nil, model.NewError(model.CodeVersion, path+".visibility", err.Error())
		}
		alpha, err := parseAlpha(*item.PixelEncoding.Alpha)
		if err != nil {
			return nil, model.NewError(model.CodeVersion, path+".pixel_encoding.alpha", err.Error())
		}
		raw, err := decodePixels(*item.RGBA, path+".rgba")
		if err != nil {
			return nil, err
		}
		scene.Layers = append(scene.Layers, model.Layer{
			ID:              *item.LayerID,
			Name:            item.DisplayName,
			Visible:         visible,
			Opacity:         *item.Opacity,
			SourceAlphaMode: alpha,
			Pixels:          compose.NormalizePixels(raw, alpha),
		})
	}
	if err := model.ValidateScene(scene); err != nil {
		return nil, err
	}
	return scene, nil
}

type v3Doc struct {
	Spec        *string            `json:"spec"`
	Dimensions  *v3Dimensions      `json:"dimensions"`
	Time        *v3Time            `json:"time"`
	SampleDepth *int               `json:"sample_depth"`
	Layers      map[string]v3Layer `json:"layers"`
	OrderLinks  []v3Link           `json:"order_links"`
}

type v3Dimensions struct {
	Width    *int `json:"width"`
	Height   *int `json:"height"`
	Channels *int `json:"channels"`
}

type v3Time struct {
	Frames        *int   `json:"frames"`
	TicksPerFrame *int64 `json:"ticks_per_frame"`
	TickRateHz    *int64 `json:"tick_rate_hz"`
}

type v3Layer struct {
	Name    *string  `json:"name"`
	Enabled *int     `json:"enabled"`
	Opacity *float64 `json:"opacity"`
	PMA     *bool    `json:"pma"`
	Pixels  *string  `json:"pixels"`
}

type v3Link struct {
	ID    *string `json:"id"`
	Above *string `json:"above"`
}

func parseV3(data []byte) (*model.Scene, error) {
	var doc v3Doc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, model.WrapError(model.CodeDecode, "scene", "invalid v3 document", err)
	}
	if doc.Spec == nil || *doc.Spec == "" {
		return nil, model.NewError(model.CodeVersion, "spec", "required field is missing")
	}
	if doc.Dimensions == nil {
		return nil, model.NewError(model.CodeVersion, "dimensions", "required field is missing")
	}
	if doc.Dimensions.Width == nil {
		return nil, model.NewError(model.CodeVersion, "dimensions.width", "required field is missing")
	}
	if doc.Dimensions.Height == nil {
		return nil, model.NewError(model.CodeVersion, "dimensions.height", "required field is missing")
	}
	if doc.Dimensions.Channels == nil {
		return nil, model.NewError(model.CodeVersion, "dimensions.channels", "required field is missing")
	}
	if doc.Time == nil {
		return nil, model.NewError(model.CodeVersion, "time", "required field is missing")
	}
	if doc.Time.Frames == nil {
		return nil, model.NewError(model.CodeVersion, "time.frames", "required field is missing")
	}
	if doc.Time.TicksPerFrame == nil {
		return nil, model.NewError(model.CodeVersion, "time.ticks_per_frame", "required field is missing")
	}
	if doc.Time.TickRateHz == nil {
		return nil, model.NewError(model.CodeVersion, "time.tick_rate_hz", "required field is missing")
	}
	if *doc.Time.TickRateHz <= 0 {
		return nil, model.NewError(model.CodeParam, "time.tick_rate_hz", "must be > 0")
	}
	if *doc.Time.TicksPerFrame <= 0 || *doc.Time.TicksPerFrame > (1<<63-1)/1_000_000_000 {
		return nil, model.NewError(model.CodeParam, "time.ticks_per_frame", "must be positive and safely representable in nanoseconds")
	}
	if len(doc.Layers) == 0 {
		return nil, model.NewError(model.CodeVersion, "layers", "at least one layer is required")
	}
	sampleDepth := 8
	if doc.SampleDepth != nil {
		sampleDepth = *doc.SampleDepth
	}
	if sampleDepth != 8 && sampleDepth != 16 {
		return nil, model.NewError(model.CodeVersion, "sample_depth", "must be 8 or 16")
	}
	if *doc.Dimensions.Width <= 0 || *doc.Dimensions.Height <= 0 {
		return nil, model.NewError(model.CodeParam, "dimensions", "width and height must be > 0")
	}
	if *doc.Dimensions.Width > model.MaxDimension || *doc.Dimensions.Height > model.MaxDimension {
		return nil, model.NewError(model.CodeParam, "dimensions", fmt.Sprintf("each dimension must be <= %d", model.MaxDimension))
	}
	if *doc.Dimensions.Channels != model.ChannelsRGBA {
		return nil, model.NewError(model.CodeParam, "dimensions.channels", "only 4-channel RGBA is supported")
	}
	order, err := resolveV3Order(doc.Layers, doc.OrderLinks)
	if err != nil {
		return nil, err
	}
	frameDurationNS := *doc.Time.TicksPerFrame * int64(1_000_000_000) / *doc.Time.TickRateHz
	if frameDurationNS <= 0 {
		return nil, model.NewError(model.CodeParam, "time.ticks_per_frame", "computed duration is not positive")
	}
	scene := &model.Scene{
		SourceVersion:   "v3",
		Width:           *doc.Dimensions.Width,
		Height:          *doc.Dimensions.Height,
		Channels:        *doc.Dimensions.Channels,
		FrameCount:      *doc.Time.Frames,
		FrameDurationNS: frameDurationNS,
		OrderExpr:       "linked-above",
	}
	for _, id := range order {
		item := doc.Layers[id]
		path := "layers." + id
		if item.Name == nil {
			return nil, model.NewError(model.CodeVersion, path+".name", "required field is missing")
		}
		if item.Enabled == nil {
			return nil, model.NewError(model.CodeVersion, path+".enabled", "required field is missing")
		}
		if *item.Enabled != 0 && *item.Enabled != 1 {
			return nil, model.NewError(model.CodeVersion, path+".enabled", "must be 0 or 1")
		}
		if item.Opacity == nil {
			return nil, model.NewError(model.CodeVersion, path+".opacity", "required field is missing")
		}
		if item.PMA == nil {
			return nil, model.NewError(model.CodeVersion, path+".pma", "required field is missing")
		}
		if item.Pixels == nil {
			return nil, model.NewError(model.CodeVersion, path+".pixels", "required field is missing")
		}
		alpha := model.AlphaStraight
		if *item.PMA {
			alpha = model.AlphaPremul
		}
		raw, err := decodePixels(*item.Pixels, path+".pixels")
		if err != nil {
			return nil, err
		}
		expected := int64(*doc.Dimensions.Width) * int64(*doc.Dimensions.Height) * int64(*doc.Dimensions.Channels) * int64(sampleDepth) / 8
		if int64(len(raw)) != expected {
			return nil, model.NewError(model.CodeDecode, path+".pixels",
				fmt.Sprintf("decoded length %d, expected %d for %d-bit samples", len(raw), expected, sampleDepth))
		}
		normalized := compose.NormalizePixels(raw, alpha)
		if sampleDepth == 16 {
			normalized = compose.NormalizePixels16(raw, alpha)
		}
		scene.Layers = append(scene.Layers, model.Layer{
			ID:              id,
			Name:            *item.Name,
			Visible:         *item.Enabled == 1,
			Opacity:         *item.Opacity,
			SourceAlphaMode: alpha,
			Pixels:          normalized,
		})
	}
	if err := model.ValidateScene(scene); err != nil {
		return nil, err
	}
	return scene, nil
}

func resolveV3Order(layers map[string]v3Layer, links []v3Link) ([]string, error) {
	if len(links) != len(layers) {
		return nil, model.NewError(model.CodeVersion, "order_links", "must contain exactly one link per layer")
	}
	above := make(map[string]*string, len(links))
	hasChild := make(map[string]bool, len(links))
	ids := make([]string, 0, len(links))
	for i, link := range links {
		path := fmt.Sprintf("order_links[%d]", i)
		if link.ID == nil || *link.ID == "" {
			return nil, model.NewError(model.CodeVersion, path+".id", "required field is missing")
		}
		if _, ok := layers[*link.ID]; !ok {
			return nil, model.NewError(model.CodeVersion, path+".id", "references unknown layer")
		}
		if _, ok := above[*link.ID]; ok {
			return nil, model.NewError(model.CodeVersion, path+".id", "duplicate layer link")
		}
		above[*link.ID] = link.Above
		ids = append(ids, *link.ID)
		if link.Above != nil {
			if _, ok := layers[*link.Above]; !ok {
				return nil, model.NewError(model.CodeVersion, path+".above", "references unknown layer")
			}
			if hasChild[*link.Above] {
				return nil, model.NewError(model.CodeVersion, path+".above", "layer has multiple successors")
			}
			hasChild[*link.Above] = true
		}
	}
	var bottoms []string
	for _, id := range ids {
		if above[id] == nil {
			bottoms = append(bottoms, id)
		}
	}
	if len(bottoms) != 1 {
		if first := firstCycle(ids, above); first != "" {
			e := model.NewError(model.CodeCycle, "order_links", "cyclic layer reference")
			e.LayerID = first
			return nil, e
		}
		return nil, model.NewError(model.CodeVersion, "order_links", "exactly one bottom layer is required")
	}
	order := make([]string, 0, len(ids))
	current := bottoms[0]
	seen := make(map[string]bool, len(ids))
	for current != "" {
		if seen[current] {
			e := model.NewError(model.CodeCycle, "order_links", "cyclic layer reference")
			e.LayerID = current
			return nil, e
		}
		seen[current] = true
		order = append(order, current)
		next := ""
		for _, id := range ids {
			if above[id] != nil && *above[id] == current {
				next = id
				break
			}
		}
		current = next
	}
	if len(order) != len(ids) {
		if first := firstCycle(ids, above); first != "" {
			e := model.NewError(model.CodeCycle, "order_links", "cyclic layer reference")
			e.LayerID = first
			return nil, e
		}
		return nil, model.NewError(model.CodeVersion, "order_links", "link chain does not cover every layer")
	}
	return order, nil
}

func firstCycle(ids []string, above map[string]*string) string {
	state := make(map[string]int, len(ids))
	stack := make([]string, 0, len(ids))
	var first string
	var walk func(string) bool
	walk = func(id string) bool {
		if state[id] == 1 {
			for i, item := range stack {
				if item == id {
					first = stack[i]
					return true
				}
			}
			first = id
			return true
		}
		if state[id] == 2 {
			return false
		}
		state[id] = 1
		stack = append(stack, id)
		if parent := above[id]; parent != nil && *parent != "" {
			if walk(*parent) {
				return true
			}
		}
		stack = stack[:len(stack)-1]
		state[id] = 2
		return false
	}
	for _, id := range ids {
		if walk(id) {
			return first
		}
	}
	return ""
}

func parseAlpha(raw string) (model.AlphaMode, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "straight", "unpremultiplied", "unassociated":
		return model.AlphaStraight, nil
	case "premultiplied", "premult", "associated":
		return model.AlphaPremul, nil
	default:
		return "", fmt.Errorf("unsupported alpha convention %q", raw)
	}
}

func parseV2Visibility(raw string) (bool, error) {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "VISIBLE", "SHOWN", "ON":
		return true, nil
	case "HIDDEN", "OFF":
		return false, nil
	default:
		return false, fmt.Errorf("unsupported visibility %q", raw)
	}
}

func decodePixels(raw, path string) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, model.WrapError(model.CodeDecode, path, "invalid pixel base64", err)
	}
	return decoded, nil
}

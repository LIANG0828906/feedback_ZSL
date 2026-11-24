package ui

import (
	"image/color"
	"math"
	"sort"
	"strconv"
	"sync"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/widget"

	"performanceexpressions/workspace/domain"
	"performanceexpressions/workspace/engine"
)

type PointMoveFunc func(segmentID, channelID, layerID string, pointIndex int, beat, value float64) error

type TimelineData struct {
	Project       domain.Project
	Result        domain.ResultSet
	SegmentID     string
	ChannelID     string
	LayerID       string
	PlayheadBeat  float64
	Interpolation string
}

type Timeline struct {
	widget.BaseWidget

	mu       sync.Mutex
	data     TimelineData
	onMove   PointMoveFunc
	onStatus func(string)

	width, height float32
	dragLayer     string
	dragChannel   string
	dragPoint     int
	dragging      bool
}

func NewTimeline(onMove PointMoveFunc, onStatus func(string)) *Timeline {
	t := &Timeline{onMove: onMove, onStatus: onStatus, dragPoint: -1}
	t.ExtendBaseWidget(t)
	return t
}

func (t *Timeline) SetData(data TimelineData) {
	t.mu.Lock()
	t.data = data
	t.mu.Unlock()
	t.Refresh()
}

func (t *Timeline) SetPlayhead(beat float64) {
	t.mu.Lock()
	t.data.PlayheadBeat = beat
	t.mu.Unlock()
	t.Refresh()
}

func (t *Timeline) CreateRenderer() fyne.WidgetRenderer {
	r := &timelineRenderer{timeline: t, box: newUnlaidContainer()}
	r.rebuild()
	return r
}

func (t *Timeline) MinSize() fyne.Size {
	return fyne.NewSize(640, 360)
}

func (t *Timeline) Tapped(event *fyne.PointEvent) {
	t.mu.Lock()
	index, layerID, channelID := t.hitPointLocked(event.Position)
	t.dragPoint = index
	t.dragLayer = layerID
	t.dragChannel = channelID
	t.mu.Unlock()
	t.Refresh()
}

func (t *Timeline) Dragged(event *fyne.DragEvent) {
	t.mu.Lock()
	if !t.dragging {
		index, layerID, channelID := t.hitPointLocked(event.Position)
		if index < 0 {
			t.mu.Unlock()
			return
		}
		t.dragPoint = index
		t.dragLayer = layerID
		t.dragChannel = channelID
		t.dragging = true
	}
	data := t.data
	segment, _, ok := data.Project.SegmentByID(data.SegmentID)
	if !ok {
		t.mu.Unlock()
		return
	}
	beat, value := t.positionToValueLocked(event.Position, segment)
	index := t.dragPoint
	layerID := t.dragLayer
	channelID := t.dragChannel
	t.mu.Unlock()
	if t.onMove == nil {
		return
	}
	if err := t.onMove(data.SegmentID, channelID, layerID, index, beat, value); err != nil {
		if t.onStatus != nil {
			t.onStatus(err.Error())
		}
	}
}

func (t *Timeline) DragEnd() {
	t.mu.Lock()
	t.dragging = false
	t.dragPoint = -1
	t.mu.Unlock()
	t.Refresh()
}

func (t *Timeline) Layout(size fyne.Size) {
	t.mu.Lock()
	t.width, t.height = size.Width, size.Height
	t.mu.Unlock()
}

func (t *Timeline) hitPointLocked(pos fyne.Position) (int, string, string) {
	seg, _, ok := t.data.Project.SegmentByID(t.data.SegmentID)
	if !ok {
		return -1, "", ""
	}
	ch, _, ok := seg.ChannelByID(t.data.ChannelID)
	if !ok {
		return -1, "", ""
	}
	layerIndex := 0
	for i, layer := range ch.Layers {
		if layer.ID == t.data.LayerID {
			layerIndex = i
			break
		}
	}
	layer := ch.Layers[layerIndex]
	best := -1
	bestDist := float64(16)
	for i, point := range layer.Points {
		x, y := t.pointToScreenLocked(point.Beat, point.Value, seg)
		dx, dy := float64(pos.X-x), float64(pos.Y-y)
		dist := math.Hypot(dx, dy)
		if dist < bestDist {
			bestDist = dist
			best = i
		}
	}
	return best, layer.ID, ch.ID
}

func (t *Timeline) positionToValueLocked(pos fyne.Position, seg domain.Segment) (float64, float64) {
	lo, hi := yRange(t.data)
	plotW := math.Max(1, float64(t.width-2*plotPadX))
	plotH := math.Max(1, float64(t.height-2*plotPadY))
	tx := (float64(pos.X) - float64(plotPadX)) / plotW
	tx = clampUnit(tx)
	ty := (float64(pos.Y) - float64(plotPadY)) / plotH
	ty = clampUnit(ty)
	beat := seg.StartBeat + tx*(seg.EndBeat-seg.StartBeat)
	value := hi - ty*(hi-lo)
	return roundBeat(beat), roundValue(value)
}

func (t *Timeline) pointToScreenLocked(beat, value float64, seg domain.Segment) (float32, float32) {
	lo, hi := yRange(t.data)
	span := math.Max(1e-9, seg.EndBeat-seg.StartBeat)
	tx := (beat - seg.StartBeat) / span
	ty := (hi - value) / math.Max(1e-9, hi-lo)
	x := plotPadX + float32(tx)*(t.width-2*plotPadX)
	y := plotPadY + float32(ty)*(t.height-2*plotPadY)
	return x, y
}

const (
	plotPadX float32 = 54
	plotPadY float32 = 28
)

func yRange(data TimelineData) (float64, float64) {
	lo, hi := 0.0, 1.0
	for _, seg := range data.Project.Segments {
		if seg.ID != data.SegmentID {
			continue
		}
		for _, ch := range seg.Channels {
			for _, layer := range ch.Layers {
				for _, point := range layer.Points {
					lo = math.Min(lo, point.Value)
					hi = math.Max(hi, point.Value)
				}
			}
		}
	}
	for _, seg := range data.Result.Segments {
		if seg.SegmentID != data.SegmentID {
			continue
		}
		for _, sample := range seg.Samples {
			for _, value := range sample.Values {
				lo = math.Min(lo, value)
				hi = math.Max(hi, value)
			}
		}
	}
	pad := math.Max(0.1, (hi-lo)*0.12)
	return lo - pad, hi + pad
}

type timelineRenderer struct {
	timeline *Timeline
	box      *fyne.Container
}

func (r *timelineRenderer) Layout(size fyne.Size) {
	r.timeline.Layout(size)
	r.box.Resize(size)
	r.rebuild()
}

func (r *timelineRenderer) MinSize() fyne.Size { return fyne.NewSize(640, 360) }

func (r *timelineRenderer) Refresh() {
	r.rebuild()
	canvas.Refresh(r.box)
}

func (r *timelineRenderer) Objects() []fyne.CanvasObject { return []fyne.CanvasObject{r.box} }

func (r *timelineRenderer) Destroy() {}

func (r *timelineRenderer) rebuild() {
	r.timeline.mu.Lock()
	data := r.timeline.data
	width, height := r.timeline.width, r.timeline.height
	dragLayer, dragPoint := r.timeline.dragLayer, r.timeline.dragPoint
	r.timeline.mu.Unlock()
	r.box.Objects = nil
	if width <= 0 || height <= 0 {
		return
	}
	background := canvas.NewRectangle(color.NRGBA{R: 20, G: 24, B: 32, A: 255})
	background.Resize(fyne.NewSize(width, height))
	r.box.Add(background)
	seg, _, ok := data.Project.SegmentByID(data.SegmentID)
	if !ok {
		r.addText("No segment selected", 16, 16, color.NRGBA{R: 220, G: 220, B: 225, A: 255})
		r.box.Refresh()
		return
	}
	lo, hi := yRange(data)
	grid := color.NRGBA{R: 54, G: 62, B: 78, A: 255}
	beats := int(math.Ceil(seg.EndBeat - seg.StartBeat))
	for i := 0; i <= beats; i++ {
		beat := seg.StartBeat + float64(i)
		x := r.screenX(beat, seg)
		line := canvas.NewLine(grid)
		line.StrokeWidth = 1
		line.Position1 = fyne.NewPos(x, plotPadY)
		line.Position2 = fyne.NewPos(x, height-plotPadY)
		r.box.Add(line)
		r.addText(trimFloat(beat), x+3, height-plotPadY+5, color.NRGBA{R: 150, G: 160, B: 176, A: 255})
	}
	for i := 0; i <= 4; i++ {
		value := lo + (hi-lo)*float64(i)/4
		y := r.screenY(value, seg)
		line := canvas.NewLine(grid)
		line.Position1 = fyne.NewPos(plotPadX, y)
		line.Position2 = fyne.NewPos(width-plotPadX, y)
		r.box.Add(line)
		r.addText(fmtValue(value), 5, y-7, color.NRGBA{R: 150, G: 160, B: 176, A: 255})
	}

	ch, _, ok := seg.ChannelByID(data.ChannelID)
	if ok {
		mode := data.Interpolation
		if mode == "" {
			mode = domain.InterpolationLinear
		}
		for _, layer := range ch.Layers {
			layerColor := color.NRGBA{R: 104, G: 164, B: 255, A: 210}
			if layer.ID != data.LayerID {
				layerColor = color.NRGBA{R: 104, G: 164, B: 255, A: 90}
			}
			for i := 0; i < 96; i++ {
				b0 := seg.StartBeat + (seg.EndBeat-seg.StartBeat)*float64(i)/95
				b1 := seg.StartBeat + (seg.EndBeat-seg.StartBeat)*float64(i+1)/95
				v0, err0 := engine.Interpolate(layer.Points, b0, mode)
				v1, err1 := engine.Interpolate(layer.Points, b1, mode)
				if err0 != nil || err1 != nil {
					break
				}
				line := canvas.NewLine(layerColor)
				line.StrokeWidth = 1
				line.Position1 = fyne.NewPos(r.screenX(b0, seg), r.screenY(v0, seg))
				line.Position2 = fyne.NewPos(r.screenX(b1, seg), r.screenY(v1, seg))
				r.box.Add(line)
			}
		}
	}
	if resultSeg, found := resultSegment(data.Result, seg.ID); found {
		var previous fyne.Position
		havePrevious := false
		for _, sample := range resultSeg.Samples {
			value, ok := sample.Values[ch.Target]
			if !ok {
				continue
			}
			pos := fyne.NewPos(r.screenX(sample.Beat, seg), r.screenY(value, seg))
			if havePrevious {
				line := canvas.NewLine(color.NRGBA{R: 255, G: 176, B: 86, A: 255})
				line.StrokeWidth = 2.5
				line.Position1 = previous
				line.Position2 = pos
				r.box.Add(line)
			}
			previous = pos
			havePrevious = true
		}
	}
	if ok {
		for i, layer := range ch.Layers {
			for j, point := range layer.Points {
				x := r.screenX(point.Beat, seg)
				y := r.screenY(point.Value, seg)
				fill := color.NRGBA{R: 118, G: 220, B: 180, A: 255}
				if layer.ID != data.LayerID {
					fill = color.NRGBA{R: 76, G: 144, B: 120, A: 210}
				}
				if layer.ID == dragLayer && j == dragPoint {
					fill = color.NRGBA{R: 255, G: 224, B: 110, A: 255}
				}
				dot := canvas.NewCircle(fill)
				dot.Resize(fyne.NewSize(9, 9))
				dot.Move(fyne.NewPos(x-4.5, y-4.5))
				r.box.Add(dot)
				_ = i
			}
		}
	}
	playhead := canvas.NewLine(color.NRGBA{R: 255, G: 94, B: 120, A: 255})
	playhead.StrokeWidth = 2
	px := r.screenX(data.PlayheadBeat, seg)
	playhead.Position1 = fyne.NewPos(px, plotPadY)
	playhead.Position2 = fyne.NewPos(px, height-plotPadY)
	r.box.Add(playhead)
	r.addText(seg.ID+" / "+data.ChannelID+" / "+data.LayerID, 12, 6, color.NRGBA{R: 215, G: 220, B: 230, A: 255})
	r.box.Refresh()
}

func (r *timelineRenderer) screenX(beat float64, seg domain.Segment) float32 {
	span := math.Max(1e-9, seg.EndBeat-seg.StartBeat)
	t := (beat - seg.StartBeat) / span
	return plotPadX + float32(t)*(r.timeline.width-2*plotPadX)
}

func (r *timelineRenderer) screenY(value float64, seg domain.Segment) float32 {
	lo, hi := yRange(r.timeline.data)
	t := (hi - value) / math.Max(1e-9, hi-lo)
	return plotPadY + float32(t)*(r.timeline.height-2*plotPadY)
}

func (r *timelineRenderer) addText(text string, x, y float32, c color.Color) {
	label := canvas.NewText(text, c)
	label.TextSize = 11
	label.Move(fyne.NewPos(x, y))
	r.box.Add(label)
}

func newUnlaidContainer() *fyne.Container {
	return fyne.NewContainerWithoutLayout()
}

func resultSegment(result domain.ResultSet, id string) (domain.SegmentResult, bool) {
	for _, seg := range result.Segments {
		if seg.SegmentID == id {
			return seg, true
		}
	}
	return domain.SegmentResult{}, false
}

func clampUnit(v float64) float64 {
	return math.Max(0, math.Min(1, v))
}

func roundBeat(v float64) float64 {
	return math.Round(v*1000) / 1000
}

func roundValue(v float64) float64 {
	return math.Round(v*10000) / 10000
}

func trimFloat(v float64) string {
	return fmtValue(v)
}

func fmtValue(v float64) string {
	return strconvFormat(v)
}

func strconvFormat(v float64) string {
	if math.Abs(v-math.Round(v)) < 1e-9 {
		return formatInt(int64(math.Round(v)))
	}
	return formatFloat(v)
}

func formatInt(v int64) string {
	return strconv.FormatInt(v, 10)
}

func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', 2, 64)
}

func sortedStrings(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

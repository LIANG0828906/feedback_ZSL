package ui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"

	"performanceexpressions/workspace/checkpoint"
	"performanceexpressions/workspace/domain"
	"performanceexpressions/workspace/engine"
	"performanceexpressions/workspace/session"
	"performanceexpressions/workspace/workbench"
)

type Workspace struct {
	app    fyne.App
	window fyne.Window

	store   *checkpoint.Store
	session *session.Session
	params  domain.Parameters
	base    domain.Project

	segmentSelect *widget.Select
	channelSelect *widget.Select
	layerSelect   *widget.Select
	faultSelect   *widget.Select
	batchEntry    *widget.Entry
	startEntry    *widget.Entry
	playButton    *widget.Button
	statusLabel   *widget.Label
	stateLabel    *widget.Label
	priorityLabel *widget.Label
	timeline      *Timeline

	selectedSegment string
	selectedChannel string
	selectedLayer   string
	suppressSelect  bool

	layerSeq int
	batchSeq int
	playhead float64
	playAnim *fyne.Animation
	playProg float32
	playing  bool
	closed   bool
}

func NewWorkspace(a fyne.App, base domain.Project, params domain.Parameters, storePath string) (*Workspace, error) {
	if params.Interpolation == "" {
		params = domain.DefaultParameters()
	}
	if err := engine.ValidateProject(base, engine.ValidateOptions{RequireTruth: true}); err != nil {
		return nil, err
	}
	if storePath == "" {
		storePath = "artifacts/ui-workspace.db"
	}
	store, err := checkpoint.Open(storePath)
	if err != nil {
		return nil, err
	}
	sess, _, err := workbench.Recover(context.Background(), store, base, params)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	w := &Workspace{
		app:      a,
		store:    store,
		session:  sess,
		params:   params,
		base:     domain.CloneProject(base),
		playhead: base.Segments[0].StartBeat,
	}
	w.selectedSegment = base.Segments[0].ID
	if ch, _, ok := base.Segments[0].ChannelByID("velocity.main"); ok {
		w.selectedChannel = ch.ID
		if len(ch.Layers) > 0 {
			w.selectedLayer = ch.Layers[0].ID
		}
	} else if len(base.Segments[0].Channels) > 0 {
		w.selectedChannel = base.Segments[0].Channels[0].ID
		if len(base.Segments[0].Channels[0].Layers) > 0 {
			w.selectedLayer = base.Segments[0].Channels[0].Layers[0].ID
		}
	}
	w.build()
	return w, nil
}

func (w *Workspace) Window() fyne.Window { return w.window }

func (w *Workspace) build() {
	w.statusLabel = widget.NewLabel("Ready. Drag a control point; edits stay in the working copy until commit.")
	w.statusLabel.Wrapping = fyne.TextWrapWord
	w.stateLabel = widget.NewLabel("")
	w.stateLabel.Wrapping = fyne.TextWrapWord
	w.priorityLabel = widget.NewLabel("")
	w.priorityLabel.TextStyle = fyne.TextStyle{Monospace: true}
	w.priorityLabel.Wrapping = fyne.TextWrapWord

	w.segmentSelect = widget.NewSelect(nil, func(string) { w.onSelectorChanged() })
	w.channelSelect = widget.NewSelect(nil, func(string) { w.onSelectorChanged() })
	w.layerSelect = widget.NewSelect(nil, func(string) { w.onSelectorChanged() })
	w.faultSelect = widget.NewSelect([]string{
		"none", "priority_cycle", "position_out_of_range", "batch_conflict", "commit_failure",
	}, func(string) {})
	w.faultSelect.SetSelected("none")
	w.batchEntry = widget.NewEntry()
	w.batchEntry.SetText("ui-batch-1")
	w.startEntry = widget.NewEntry()
	w.startEntry.SetText("0")
	w.playButton = widget.NewButton("Play", w.togglePlayback)
	w.timeline = NewTimeline(w.movePoint, func(message string) {
		w.statusLabel.SetText(message)
	})
	w.window = w.app.NewWindow("Performance Expression Workbench")
	w.window.Resize(fyne.NewSize(1420, 880))

	addLayer := widget.NewButton("Add layer", w.addLayer)
	removeLayer := widget.NewButton("Remove layer", w.removeLayer)
	promote := widget.NewButton("Promote channel", w.promoteChannel)
	clearFault := widget.NewButton("Clear fault", w.clearFault)
	injectFault := widget.NewButton("Inject fault", w.injectFault)
	commit := widget.NewButton("Commit segment batch", w.commit)
	validate := widget.NewButton("Validate working", w.validate)
	pause := widget.NewButton("Pause", w.pausePlayback)

	top := container.NewVBox(
		container.NewHBox(
			widget.NewLabelWithStyle("Project:", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
			widget.NewLabel(w.session.ProjectID()),
			widget.NewLabelWithStyle("Segment:", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
			w.segmentSelect,
			widget.NewLabelWithStyle("Channel:", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
			w.channelSelect,
			widget.NewLabelWithStyle("Layer:", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
			w.layerSelect,
		),
		container.NewHBox(
			addLayer, removeLayer, promote, validate,
			widget.NewSeparator(),
			widget.NewLabel("Batch:"), w.batchEntry, commit,
		),
		container.NewHBox(
			widget.NewLabel("Start beat:"), w.startEntry, w.playButton, pause,
			widget.NewSeparator(),
			widget.NewLabel("Fault:"), w.faultSelect, injectFault, clearFault,
		),
		widget.NewSeparator(),
	)
	side := container.NewVBox(
		widget.NewLabelWithStyle("Segment state", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		w.stateLabel,
		widget.NewSeparator(),
		widget.NewLabelWithStyle("Priority declarations", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		w.priorityLabel,
		widget.NewSeparator(),
		widget.NewLabel("Orange: committed overlay curve. Blue: working layer curves. Green: draggable points. Pink: playhead."),
	)
	sideScroll := container.NewVScroll(side)
	sideScroll.SetMinSize(fyne.NewSize(360, 560))
	content := container.NewBorder(top, w.statusLabel, nil, sideScroll, w.timeline)
	w.window.SetContent(content)
	w.window.SetCloseIntercept(func() {
		w.Close()
		w.window.Close()
	})
	w.refreshSelectors()
	w.refreshTimeline()
}

func (w *Workspace) Close() {
	if w.closed {
		return
	}
	w.closed = true
	w.pausePlayback()
	if w.store != nil {
		_ = w.store.Close()
	}
}

func (w *Workspace) refreshSelectors() {
	project := w.session.WorkingProject()
	w.suppressSelect = true
	segmentOptions := make([]string, 0, len(project.Segments))
	for _, seg := range project.Segments {
		segmentOptions = append(segmentOptions, seg.ID)
	}
	w.segmentSelect.SetOptions(segmentOptions)
	if !contains(segmentOptions, w.selectedSegment) && len(segmentOptions) > 0 {
		w.selectedSegment = segmentOptions[0]
	}
	w.segmentSelect.SetSelected(w.selectedSegment)
	seg, _, ok := project.SegmentByID(w.selectedSegment)
	if !ok {
		w.suppressSelect = false
		return
	}
	channelOptions := make([]string, 0, len(seg.Channels))
	for _, ch := range seg.Channels {
		channelOptions = append(channelOptions, ch.ID)
	}
	w.channelSelect.SetOptions(channelOptions)
	if !contains(channelOptions, w.selectedChannel) && len(channelOptions) > 0 {
		w.selectedChannel = channelOptions[0]
	}
	w.channelSelect.SetSelected(w.selectedChannel)
	ch, _, ok := seg.ChannelByID(w.selectedChannel)
	if ok {
		layerOptions := make([]string, 0, len(ch.Layers))
		for _, layer := range ch.Layers {
			layerOptions = append(layerOptions, layer.ID)
		}
		w.layerSelect.SetOptions(layerOptions)
		if !contains(layerOptions, w.selectedLayer) && len(layerOptions) > 0 {
			w.selectedLayer = layerOptions[0]
		}
		w.layerSelect.SetSelected(w.selectedLayer)
	}
	w.suppressSelect = false
	w.refreshPriority(seg)
	states := w.session.States()
	var stateText strings.Builder
	for _, state := range states {
		status := "clean"
		if state.Dirty {
			status = "dirty"
		}
		if state.Failed {
			status = "FAILED"
		}
		fmt.Fprintf(&stateText, "%s: %s", state.SegmentID, status)
		if state.LastError != nil {
			fmt.Fprintf(&stateText, " [%s]", state.LastError.Code)
		}
		stateText.WriteByte('\n')
	}
	w.stateLabel.SetText(strings.TrimSpace(stateText.String()))
}

func (w *Workspace) refreshPriority(seg domain.Segment) {
	var b strings.Builder
	fmt.Fprintf(&b, "tempo=%.1f bpm  density=%.2f  range=[%.2f, %.2f]\n", seg.TempoBPM, seg.NoteDensity, seg.StartBeat, seg.EndBeat)
	fmt.Fprintf(&b, "priority_version=%s\n", domain.PriorityVersion(seg))
	for _, rule := range seg.Priorities {
		fmt.Fprintf(&b, "%s > %s  (%s)\n", rule.Higher, rule.Lower, rule.Basis)
	}
	w.priorityLabel.SetText(strings.TrimSpace(b.String()))
}

func (w *Workspace) onSelectorChanged() {
	if w.suppressSelect {
		return
	}
	w.selectedSegment = w.segmentSelect.Selected
	w.selectedChannel = w.channelSelect.Selected
	w.selectedLayer = w.layerSelect.Selected
	w.refreshSelectors()
	seg, _, ok := w.session.WorkingProject().SegmentByID(w.selectedSegment)
	if ok && (w.playhead < seg.StartBeat || w.playhead >= seg.EndBeat) {
		w.playhead = seg.StartBeat
	}
	w.refreshTimeline()
}

func (w *Workspace) refreshTimeline() {
	working := w.session.WorkingProject()
	committedResult := w.session.CommittedResult()
	seg, _, ok := working.SegmentByID(w.selectedSegment)
	if !ok {
		return
	}
	if w.playhead < seg.StartBeat || w.playhead >= seg.EndBeat {
		w.playhead = seg.StartBeat
	}
	w.timeline.SetData(TimelineData{
		Project:       working,
		Result:        committedResult,
		SegmentID:     w.selectedSegment,
		ChannelID:     w.selectedChannel,
		LayerID:       w.selectedLayer,
		PlayheadBeat:  w.playhead,
		Interpolation: w.params.Interpolation,
	})
	diff, diffErr := w.session.DiffWorkingAgainstCommitted()
	if domain.CodeOf(diffErr) == domain.CodeDiff {
		w.statusLabel.SetText(fmt.Sprintf("Working copy differs from committed state at %d sample fields. Export and playback remain on the committed state.", diff.Differences))
	} else {
		w.statusLabel.SetText("Working copy is clean. Orange curve is committed playback output.")
	}
}

func (w *Workspace) movePoint(segmentID, channelID, layerID string, pointIndex int, beat, value float64) error {
	err := w.session.ApplyEdit(domain.EditOp{
		Kind: domain.EditMovePoint, SegmentID: segmentID, ChannelID: channelID, LayerID: layerID,
		PointIndex: pointIndex, Beat: beat, Value: value, BatchID: w.currentBatch(),
	})
	if err != nil {
		w.statusLabel.SetText(err.Error())
		return err
	}
	w.refreshTimeline()
	w.refreshSelectors()
	return nil
}

func (w *Workspace) addLayer() {
	seg, _, ok := w.session.WorkingProject().SegmentByID(w.selectedSegment)
	if !ok {
		return
	}
	ch, _, ok := seg.ChannelByID(w.selectedChannel)
	if !ok {
		return
	}
	w.layerSeq++
	layerID := fmt.Sprintf("user.layer.%d", w.layerSeq)
	mid := seg.StartBeat + (seg.EndBeat-seg.StartBeat)/2
	layer := &domain.Layer{ID: layerID, Points: []domain.ControlPoint{
		{Beat: seg.StartBeat, Value: 0.02},
		{Beat: mid, Value: 0.06},
		{Beat: seg.EndBeat, Value: 0.02},
	}}
	if err := w.session.ApplyEdit(domain.EditOp{
		Kind: domain.EditAddLayer, SegmentID: seg.ID, ChannelID: ch.ID, Layer: layer, BatchID: w.currentBatch(),
	}); err != nil {
		w.statusLabel.SetText(err.Error())
		return
	}
	w.selectedLayer = layerID
	w.refreshSelectors()
	w.refreshTimeline()
}

func (w *Workspace) removeLayer() {
	if err := w.session.ApplyEdit(domain.EditOp{
		Kind: domain.EditRemoveLayer, SegmentID: w.selectedSegment, ChannelID: w.selectedChannel,
		LayerID: w.selectedLayer, BatchID: w.currentBatch(),
	}); err != nil {
		w.statusLabel.SetText(err.Error())
		return
	}
	w.selectedLayer = ""
	w.refreshSelectors()
	w.refreshTimeline()
}

func (w *Workspace) promoteChannel() {
	seg, _, ok := w.session.WorkingProject().SegmentByID(w.selectedSegment)
	if !ok {
		return
	}
	ch, _, ok := seg.ChannelByID(w.selectedChannel)
	if !ok {
		return
	}
	var rules []domain.PriorityRule
	for _, rule := range seg.Priorities {
		sameTargetRule := false
		for _, other := range seg.Channels {
			if other.Target == ch.Target && (rule.Higher == other.ID || rule.Lower == other.ID) {
				sameTargetRule = true
				break
			}
		}
		if sameTargetRule && (rule.Higher == ch.ID || rule.Lower == ch.ID) {
			continue
		}
		rules = append(rules, rule)
	}
	for _, other := range seg.Channels {
		if other.ID == ch.ID || other.Target != ch.Target {
			continue
		}
		exists := false
		for _, rule := range rules {
			if rule.Higher == ch.ID && rule.Lower == other.ID {
				exists = true
				break
			}
		}
		if !exists {
			rules = append(rules, domain.PriorityRule{Higher: ch.ID, Lower: other.ID, Basis: "ui-promoted-" + ch.ID})
		}
	}
	if err := w.session.ApplyEdit(domain.EditOp{
		Kind: domain.EditSetPriority, SegmentID: seg.ID, Priorities: rules, BatchID: w.currentBatch(),
	}); err != nil {
		w.statusLabel.SetText(err.Error())
		return
	}
	w.refreshSelectors()
	w.refreshTimeline()
}

func (w *Workspace) injectFault() {
	kind := session.FaultKind(w.faultSelect.Selected)
	if kind == "none" || kind == "" {
		w.statusLabel.SetText("Select a fault kind first.")
		return
	}
	if err := w.session.InjectFault(session.Fault{Kind: kind, SegmentID: w.selectedSegment}); err != nil {
		w.statusLabel.SetText(err.Error())
		return
	}
	w.refreshSelectors()
	w.refreshTimeline()
	w.statusLabel.SetText("Injected " + string(kind) + " on " + w.selectedSegment + ". Commit to exercise the error chain; retry after clearing.")
}

func (w *Workspace) clearFault() {
	if err := w.session.RetrySegment(w.selectedSegment); err != nil {
		w.statusLabel.SetText(err.Error())
		return
	}
	w.refreshSelectors()
	w.refreshTimeline()
	w.statusLabel.SetText("Fault cleared; segment is retryable.")
}

func (w *Workspace) commit() {
	batchID := strings.TrimSpace(w.batchEntry.Text)
	if batchID == "" {
		batchID = w.currentBatch()
	}
	receipt, err := w.session.CommitSegment(context.Background(), batchID, w.selectedSegment)
	if err != nil {
		w.statusLabel.SetText(err.Error())
		w.refreshSelectors()
		return
	}
	w.batchSeq++
	w.batchEntry.SetText(fmt.Sprintf("ui-batch-%d", w.batchSeq+1))
	w.statusLabel.SetText(fmt.Sprintf("Committed %s as batch %s at sequence %d. Overlay cache refreshed.", receipt.SegmentIDs, receipt.BatchID, receipt.Sequence))
	w.refreshSelectors()
	w.refreshTimeline()
}

func (w *Workspace) validate() {
	project := w.session.WorkingProject()
	if err := engine.ValidateProject(project, engine.ValidateOptions{RequireTruth: false}); err != nil {
		w.statusLabel.SetText(err.Error())
		return
	}
	committed := w.session.CommittedProject()
	result := w.session.CommittedResult()
	tolerance := w.params.Tolerance
	if tolerance == 0 {
		tolerance = 1e-9
	}
	precision, err := engine.ValidatePrecision(committed, result, tolerance)
	if err != nil {
		w.statusLabel.SetText(fmt.Sprintf("Structure valid. Truth check: %v", err))
		return
	}
	w.statusLabel.SetText(fmt.Sprintf("Structure valid. Truth precision OK: %d points, max source mismatches=%d.", precision.PassedPoints, precision.SourceMismatches))
}

func (w *Workspace) togglePlayback() {
	if w.playing {
		w.pausePlayback()
		return
	}
	w.startPlayback()
}

func (w *Workspace) startPlayback() {
	seg, _, ok := w.session.CommittedProject().SegmentByID(w.selectedSegment)
	if !ok {
		return
	}
	startBeat, err := strconv.ParseFloat(strings.TrimSpace(w.startEntry.Text), 64)
	if err != nil || startBeat < seg.StartBeat || startBeat >= seg.EndBeat {
		w.statusLabel.SetText("E_POS: start beat must be inside the selected segment")
		return
	}
	if w.playAnim != nil {
		w.playAnim.Stop()
	}
	span := seg.EndBeat - startBeat
	beatsPerSecond := seg.TempoBPM / 60
	duration := time.Duration(span / beatsPerSecond * float64(time.Second))
	if duration < 50*time.Millisecond {
		duration = 50 * time.Millisecond
	}
	w.playProg = 0
	w.playhead = startBeat
	w.timeline.SetPlayhead(startBeat)
	w.playing = true
	w.playButton.SetText("Pause")
	anim := fyne.NewAnimation(duration, func(f float32) {
		w.playProg = f
		beat := startBeat + float64(f)*span
		w.playhead = beat
		w.timeline.SetPlayhead(beat)
		w.updatePlaybackStatus(beat)
	})
	anim.Curve = fyne.AnimationLinear
	anim.RepeatCount = fyne.AnimationRepeatForever
	w.playAnim = anim
	anim.Start()
}

func (w *Workspace) pausePlayback() {
	if w.playAnim != nil {
		w.playAnim.Stop()
	}
	w.playing = false
	if w.playButton != nil {
		w.playButton.SetText("Play")
	}
	if w.playhead > 0 && w.statusLabel != nil {
		w.updatePlaybackStatus(w.playhead)
	}
}

func (w *Workspace) updatePlaybackStatus(beat float64) {
	tick := int(beat*float64(w.session.CommittedProject().SamplesPerBeat) + 0.5)
	sample, err := w.session.PlaybackAtTick(w.selectedSegment, tick)
	if err != nil {
		return
	}
	parts := make([]string, 0, len(sample.Values))
	for _, target := range sortedStrings(keys(sample.Values)) {
		parts = append(parts, fmt.Sprintf("%s=%.5f@%s", target, sample.Values[target], sample.Sources[target].Channel))
	}
	w.statusLabel.SetText(fmt.Sprintf("Playback beat %.3f (tick %d): %s", beat, sample.Tick, strings.Join(parts, "  ")))
}

func (w *Workspace) currentBatch() string {
	if w.batchSeq == 0 {
		w.batchSeq = 1
	}
	return fmt.Sprintf("ui-batch-%d", w.batchSeq)
}

func keys(values map[string]float64) []string {
	out := make([]string, 0, len(values))
	for key := range values {
		out = append(out, key)
	}
	return out
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

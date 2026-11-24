//go:build fyne || windows

package desktop

import (
	"context"
	"fmt"
	"image/color"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"spatialworkbench/internal/audio"
	"spatialworkbench/internal/checkpoint"
	"spatialworkbench/internal/generator"
	"spatialworkbench/internal/loader"
	"spatialworkbench/internal/model"
	"spatialworkbench/internal/session"
	"spatialworkbench/internal/verify"
	"spatialworkbench/internal/werr"
)

const defaultManifest = "samples/workbench_samples.json"

type workbench struct {
	app    fyne.App
	window fyne.Window
	player mediaPlayer

	manifest     model.Manifest
	sample       model.Sample
	params       model.Params
	sess         *session.Session
	snapshots    []session.FrameSnapshot
	selected     int
	report       verify.Report
	haveReport   bool
	displayError *werr.Error

	manifestEntry *widget.Entry
	dbEntry       *widget.Entry
	sampleSelect  *widget.Select
	status        *widget.Label
	progress      *widget.ProgressBar
	compareLabel  *widget.Label

	frameEntry  *widget.Entry
	hopEntry    *widget.Entry
	windowEntry *widget.Entry
	quantEntry  *widget.Entry
	interpEntry *widget.Entry
	gainEntry   *widget.Entry
	tolEntry    *widget.Entry
	workerEntry *widget.Entry
	bufferEntry *widget.Entry
	routeEntry  *widget.Entry
	injectKind  *widget.Select
	injectChan  *widget.Select
	injectFrame *widget.Entry

	table      *widget.Table
	energyPlot *fyne.Container
	delayPlot  *fyne.Container
	scopePlot  *fyne.Container
	direction  *fyne.Container
}

func Run() {
	a := app.New()
	w := &workbench{app: a, window: a.NewWindow("Spatial Acoustic Reconstruction Workbench"), player: newPlayer()}
	w.window.Resize(fyne.NewSize(1440, 960))
	w.build()
	w.loadManifest(defaultManifest)
	w.window.ShowAndRun()
}

func (w *workbench) build() {
	w.status = widget.NewLabel("ready")
	w.status.Wrapping = fyne.TextWrapWord
	w.progress = widget.NewProgressBar()
	w.compareLabel = widget.NewLabel("Three paths: not compared")
	w.compareLabel.Wrapping = fyne.TextWrapWord

	w.manifestEntry = widget.NewEntry()
	w.manifestEntry.SetText(defaultManifest)
	w.dbEntry = widget.NewEntry()
	w.dbEntry.SetText(filepath.Join(".workbench", "desktop.sqlite"))
	w.sampleSelect = widget.NewSelect(nil, func(value string) {
		if value != "" {
			w.selectSample(value)
		}
	})
	loadButton := widget.NewButton("Load", func() { w.loadManifest(w.manifestEntry.Text) })
	generateButton := widget.NewButton("Regenerate", func() {
		path := w.manifestEntry.Text
		if strings.TrimSpace(path) == "" {
			path = defaultManifest
		}
		manifest := generator.Generate(generator.Options{Seed: 20261124, Samples: 3, Prefix: "local"})
		if err := generator.WriteFile(path, manifest); err != nil {
			w.showError(werr.New(werr.CodeState, "generate samples: %v", err))
			return
		}
		w.loadManifest(path)
	})
	fileRow := container.NewBorder(nil, nil, nil, container.NewHBox(loadButton, generateButton),
		container.NewGridWithColumns(4,
			widget.NewLabel("Manifest"), w.manifestEntry,
			widget.NewLabel("Checkpoint DB"), w.dbEntry,
		),
	)

	w.frameEntry = widget.NewEntry()
	w.hopEntry = widget.NewEntry()
	w.windowEntry = widget.NewEntry()
	w.quantEntry = widget.NewEntry()
	w.interpEntry = widget.NewEntry()
	w.gainEntry = widget.NewEntry()
	w.tolEntry = widget.NewEntry()
	w.workerEntry = widget.NewEntry()
	w.bufferEntry = widget.NewEntry()
	w.routeEntry = widget.NewEntry()
	paramsGrid := container.NewGridWithColumns(4,
		widget.NewLabel("Frame"), w.frameEntry,
		widget.NewLabel("Hop"), w.hopEntry,
		widget.NewLabel("Window"), w.windowEntry,
		widget.NewLabel("Quant step"), w.quantEntry,
		widget.NewLabel("Interp order"), w.interpEntry,
		widget.NewLabel("Render gain"), w.gainEntry,
		widget.NewLabel("Error tolerance"), w.tolEntry,
		widget.NewLabel("Max workers"), w.workerEntry,
		widget.NewLabel("Samples/worker"), w.bufferEntry,
		widget.NewLabel("Routing"), w.routeEntry,
	)
	applyButton := widget.NewButton("Apply to uncommitted", w.applyParams)
	reloadButton := widget.NewButton("Restart session", func() { w.startSession() })

	w.injectKind = widget.NewSelect([]string{"none", "missing", "order", "clip", "bad-frame"}, nil)
	w.injectKind.SetSelected("none")
	w.injectChan = widget.NewSelect(nil, nil)
	w.injectFrame = widget.NewEntry()
	w.injectFrame.SetText("0")
	injectButton := widget.NewButton("Apply injection", w.applyInjection)
	injectGrid := container.NewGridWithColumns(6,
		widget.NewLabel("Injection"), w.injectKind,
		widget.NewLabel("Channel"), w.injectChan,
		widget.NewLabel("Frame"), w.injectFrame,
		injectButton,
	)

	w.table = widget.NewTable(
		func() (int, int) { return len(w.snapshots), 12 },
		func() fyne.CanvasObject { return widget.NewLabel("") },
		func(id widget.TableCellID, object fyne.CanvasObject) {
			object.(*widget.Label).SetText(w.tableText(id))
		},
	)
	w.table.OnSelected = func(id widget.TableCellID) {
		w.selected = id.Row
		w.refreshDiagnostics()
	}
	widths := []float32{55, 90, 45, 70, 60, 60, 190, 220, 130, 80, 110, 90}
	for i, width := range widths {
		w.table.SetColumnWidth(i, width)
	}
	tableBox := container.NewStack(widget.NewLabel(""), container.NewGridWrap(fyne.NewSize(1280, 310), w.table))

	w.direction = container.NewWithoutLayout()
	w.energyPlot = container.NewWithoutLayout()
	w.delayPlot = container.NewWithoutLayout()
	w.scopePlot = container.NewWithoutLayout()
	diagRow := container.NewGridWithColumns(2,
		widget.NewCard("Sound-field direction", "ITD-derived direction per channel", w.plotFrame(w.direction, fyne.NewSize(650, 220))),
		widget.NewCard("Three-path waveform compare", "realtime / resume / persisted recompute", w.plotFrame(w.scopePlot, fyne.NewSize(650, 220))),
	)
	curveRow := container.NewGridWithColumns(2,
		widget.NewCard("Channel early-energy curves", "committed and uncommitted estimates", w.plotFrame(w.energyPlot, fyne.NewSize(650, 180))),
		widget.NewCard("Direct-delay curves", "per-channel arrival sample", w.plotFrame(w.delayPlot, fyne.NewSize(650, 180))),
	)

	nextButton := widget.NewButton("Estimate next", func() {
		w.runSessionAction("estimate", func() *werr.Error {
			_, err := w.sess.ProcessNext(context.Background())
			return err
		})
	})
	commitButton := widget.NewButton("Commit next", func() {
		w.runSessionAction("commit", func() *werr.Error {
			_, err := w.sess.CommitNext(context.Background())
			return err
		})
	})
	stepButton := widget.NewButton("Estimate + commit", func() {
		w.runSessionAction("step", func() *werr.Error {
			_, err := w.sess.ProcessAndCommitNext(context.Background())
			return err
		})
	})
	batchButton := widget.NewButton("Batch (bounded)", func() {
		w.runSessionAction("batch", func() *werr.Error {
			_, err := w.sess.ProcessAndCommitBatch(context.Background(), w.params.MaxWorkers*2)
			return err
		})
	})
	retryButton := widget.NewButton("Retry failed", func() {
		if w.sess == nil {
			return
		}
		w.sess.SetInjection(session.Injection{})
		failed := w.firstFailed()
		if failed < 0 {
			w.status.SetText("No failed frame")
			return
		}
		if err := w.sess.RetryFrame(failed); err != nil {
			w.showError(err)
			return
		}
		w.runSessionAction("retry", func() *werr.Error {
			_, err := w.sess.ProcessAndCommitNext(context.Background())
			return err
		})
	})
	recalcButton := widget.NewButton("Recalculate uncommitted", func() {
		w.runSessionAction("recalculate", func() *werr.Error {
			if err := w.sess.ReestimateUncommitted(w.sess.Progress().Committed); err != nil {
				return err
			}
			_, err := w.sess.ProcessAndCommitBatch(context.Background(), w.params.MaxWorkers*2)
			return err
		})
	})
	compareButton := widget.NewButton("Compare 3 paths", w.comparePaths)
	audioButton := widget.NewButton("Audition", w.audition)
	pauseButton := widget.NewButton("Pause / stop", w.pause)
	controls := container.NewHBox(nextButton, commitButton, stepButton, batchButton, retryButton, recalcButton, compareButton, audioButton, pauseButton)

	paramsCard := widget.NewCard("Frame analysis and reconstruction parameters", "Frozen values remain attached to committed frames", container.NewVBox(paramsGrid, container.NewHBox(applyButton, reloadButton), injectGrid))
	top := container.NewVBox(
		widget.NewCard("Deterministic local sample", "No external audio or network service is required", container.NewVBox(fileRow, container.NewGridWithColumns(2, widget.NewLabel("Sample"), w.sampleSelect))),
		paramsCard,
		controls,
		container.NewGridWithColumns(2, w.progress, w.status),
	)
	body := container.NewVBox(tableBox, diagRow, curveRow, widget.NewCard("Diagnostics log", "", w.compareLabel))
	scroll := container.NewScroll(body)
	w.window.SetContent(container.NewBorder(top, nil, nil, nil, scroll))
}

func (w *workbench) plotFrame(view *fyne.Container, size fyne.Size) fyne.CanvasObject {
	background := canvas.NewRectangle(color.NRGBA{R: 18, G: 23, B: 31, A: 255})
	background.SetMinSize(size)
	view.Resize(size)
	return container.NewStack(background, view)
}

func (w *workbench) loadManifest(path string) {
	if strings.TrimSpace(path) == "" {
		path = defaultManifest
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		manifest := generator.Generate(generator.Options{Seed: 20261124, Samples: 3, Prefix: "local"})
		if err := generator.WriteFile(path, manifest); err != nil {
			w.showError(werr.New(werr.CodeState, "generate default manifest: %v", err))
			return
		}
	}
	manifest, err := loader.LoadFile(path)
	if err != nil {
		w.showError(err)
		return
	}
	w.manifest = manifest
	options := make([]string, 0, len(manifest.Samples))
	for _, sample := range manifest.Samples {
		options = append(options, sample.ID)
	}
	w.sampleSelect.SetOptions(options)
	if len(options) > 0 {
		w.sampleSelect.SetSelected(options[0])
	}
}

func (w *workbench) selectSample(id string) {
	for _, sample := range w.manifest.Samples {
		if sample.ID == id {
			w.sample = sample
			w.params = model.DefaultParams(sample)
			w.fillParams(w.params)
			channels := append([]string(nil), sample.Layout.Channels...)
			w.injectChan.SetOptions(channels)
			if len(channels) > 0 {
				w.injectChan.SetSelected(channels[0])
			}
			w.startSession()
			return
		}
	}
}

func (w *workbench) fillParams(params model.Params) {
	w.frameEntry.SetText(strconv.Itoa(params.FrameLength))
	w.hopEntry.SetText(strconv.Itoa(params.Hop))
	w.windowEntry.SetText(strconv.Itoa(params.WindowLength))
	w.quantEntry.SetText(strconv.Itoa(params.QuantStep))
	w.interpEntry.SetText(strconv.Itoa(params.InterpOrder))
	w.gainEntry.SetText(fmt.Sprintf("%g", params.RenderGain))
	w.tolEntry.SetText(fmt.Sprintf("%g", params.ErrorTolerance))
	w.workerEntry.SetText(strconv.Itoa(params.MaxWorkers))
	w.bufferEntry.SetText(strconv.Itoa(params.BufferSamplesPerWorker))
	w.routeEntry.SetText(formatRouting(params.Routing, w.sample.Layout.Channels))
}

func (w *workbench) paramsFromEntries() (model.Params, *werr.Error) {
	base := model.DefaultParams(w.sample)
	var err *werr.Error
	if base.FrameLength, err = entryInt(w.frameEntry, "frameLength"); err != nil {
		return base, err
	}
	if base.Hop, err = entryInt(w.hopEntry, "hop"); err != nil {
		return base, err
	}
	if base.WindowLength, err = entryInt(w.windowEntry, "windowLength"); err != nil {
		return base, err
	}
	if base.QuantStep, err = entryInt(w.quantEntry, "quantStep"); err != nil {
		return base, err
	}
	if base.InterpOrder, err = entryInt(w.interpEntry, "interpOrder"); err != nil {
		return base, err
	}
	if base.RenderGain, err = entryFloat(w.gainEntry, "renderGain"); err != nil {
		return base, err
	}
	if base.ErrorTolerance, err = entryFloat(w.tolEntry, "errorTolerance"); err != nil {
		return base, err
	}
	if base.MaxWorkers, err = entryInt(w.workerEntry, "maxWorkers"); err != nil {
		return base, err
	}
	if base.BufferSamplesPerWorker, err = entryInt(w.bufferEntry, "bufferSamplesPerWorker"); err != nil {
		return base, err
	}
	if strings.TrimSpace(w.routeEntry.Text) != "" {
		routing, routeErr := parseRouting(w.routeEntry.Text)
		if routeErr != nil {
			return base, routeErr
		}
		base.Routing = routing
	}
	if validateErr := base.Validate(w.sample); validateErr != nil {
		return base, validateErr
	}
	return base, nil
}

func (w *workbench) startSession() {
	if w.sess != nil {
		w.sess.Close()
		w.sess = nil
	}
	params, paramsErr := w.paramsFromEntries()
	if paramsErr != nil {
		w.showError(paramsErr)
		return
	}
	store, storeErr := checkpoint.Open(w.dbEntry.Text)
	if storeErr != nil {
		w.showError(storeErr)
		return
	}
	s, openErr := session.Open(w.sample, params, store, w.injection())
	if openErr != nil {
		store.Close()
		w.showError(openErr)
		return
	}
	w.params = params
	w.sess = s
	w.haveReport = false
	w.report = verify.Report{}
	info := s.ResumeInfo()
	message := fmt.Sprintf("Loaded %s, %d frames", w.sample.ID, params.TotalFrames(len(w.sample.Dry.Samples)))
	if info.Loaded > 0 {
		message += fmt.Sprintf("; resumed %d committed frame(s)", info.Loaded)
	}
	if info.Invalidated {
		message += "; checkpoint invalidated: " + info.Reason
	}
	w.status.SetText(message)
	w.refresh()
}

func (w *workbench) applyParams() {
	if w.sess == nil {
		return
	}
	params, err := w.paramsFromEntries()
	if err != nil {
		w.showError(err)
		return
	}
	progress := w.sess.Progress()
	if err := w.sess.UpdateUncommittedParams(progress.Committed, params); err != nil {
		w.showError(err)
		return
	}
	w.params = params
	w.status.SetText("Parameters applied only to uncommitted frames")
	w.refresh()
}

func (w *workbench) applyInjection() {
	if w.sess == nil {
		w.startSession()
		if w.sess == nil {
			return
		}
	}
	injection := w.injection()
	if injection.MissingChannel != "" {
		w.startSession()
		return
	}
	w.sess.SetInjection(injection)
	w.status.SetText("Injection armed for the next affected frame")
}

func (w *workbench) injection() session.Injection {
	kind := w.injectKind.Selected
	channel := w.injectChan.Selected
	frame, _ := strconv.Atoi(strings.TrimSpace(w.injectFrame.Text))
	if frame < 0 {
		frame = 0
	}
	injection := session.Injection{}
	switch kind {
	case "missing":
		injection.MissingChannel = channel
	case "order":
		injection.InvertArrivalChannel = channel
		injection.InvertArrivalFrame = &frame
	case "clip":
		injection.GainOverflowChannel = channel
		injection.GainOverflowFrame = &frame
	case "bad-frame":
		injection.BadFrame = &frame
	}
	return injection
}

func (w *workbench) runSessionAction(name string, action func() *werr.Error) {
	if w.sess == nil {
		return
	}
	err := action()
	if err != nil {
		w.showError(err)
	} else {
		w.status.SetText(name + " completed; progress " + progressText(w.sess.Progress()))
	}
	w.refresh()
}

func (w *workbench) firstFailed() int {
	for _, frame := range w.snapshots {
		if frame.State == session.StateFailed {
			return frame.Index
		}
	}
	return -1
}

func (w *workbench) comparePaths() {
	if w.sess == nil {
		return
	}
	dir, err := os.MkdirTemp("", "spatial-workbench-ui-*")
	if err != nil {
		w.showError(werr.New(werr.CodeState, "create comparison directory: %v", err))
		return
	}
	defer os.RemoveAll(dir)
	report, compareErr := verify.RunThreePaths(context.Background(), w.sample, w.params, dir, w.sess.Progress().Committed)
	if compareErr != nil {
		w.showError(compareErr)
	}
	w.report = report
	w.haveReport = true
	lines := []string{}
	if compareErr == nil {
		lines = append(lines, "PASS: realtime, resume, and persisted recompute are sample-for-sample identical.")
	} else {
		lines = append(lines, "FAIL: "+compareErr.Error())
	}
	for _, comparison := range report.Compared {
		state := "PASS"
		if !comparison.Equal {
			state = "FAIL"
		}
		lines = append(lines, fmt.Sprintf("%s %s == %s %s", state, comparison.Left, comparison.Right, comparison.Error))
	}
	w.compareLabel.SetText(strings.Join(lines, "\n"))
	w.refresh()
}

func (w *workbench) audition() {
	if w.sess == nil {
		return
	}
	var channels [][]float64
	if w.sess.Progress().Committed == w.sess.Progress().Total {
		output, err := w.sess.Output()
		if err != nil {
			w.showError(err)
			return
		}
		channels = output.Output
	} else {
		w.refresh()
		if w.selected < 0 || w.selected >= len(w.snapshots) || w.snapshots[w.selected].Result == nil {
			w.status.SetText("Select an estimated frame or finish all frames before auditioning")
			return
		}
		channels = w.snapshots[w.selected].Result.Render
	}
	dir, err := os.MkdirTemp("", "spatial-workbench-audio-*")
	if err != nil {
		w.showError(werr.New(werr.CodeState, "create audio directory: %v", err))
		return
	}
	path := filepath.Join(dir, "audition.wav")
	file, err := os.Create(path)
	if err != nil {
		w.showError(werr.New(werr.CodeState, "create audition WAV: %v", err))
		return
	}
	writeErr := audio.WriteFloat32WAV(file, w.sample.SampleRate, channels)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		w.showError(werr.New(werr.CodeState, "write audition WAV: %v %v", writeErr, closeErr))
		return
	}
	if err := w.player.Play(path); err != nil {
		w.showError(werr.New(werr.CodeState, "play audition: %v", err))
		return
	}
	w.status.SetText("Audition playing: " + path)
}

func (w *workbench) pause() {
	if err := w.player.Pause(); err != nil {
		w.showError(werr.New(werr.CodeState, "pause audition: %v", err))
		return
	}
	w.status.SetText("Audition paused")
}

func (w *workbench) refresh() {
	if w.sess == nil {
		return
	}
	w.snapshots = w.sess.Frames()
	progress := w.sess.Progress()
	if progress.Total > 0 {
		w.progress.SetValue(float64(progress.Committed) / float64(progress.Total))
	}
	w.table.Refresh()
	w.refreshDiagnostics()
}

func (w *workbench) refreshDiagnostics() {
	if len(w.snapshots) == 0 {
		return
	}
	if w.selected < 0 || w.selected >= len(w.snapshots) {
		w.selected = 0
	}
	w.drawDirection()
	w.drawEnergyCurve()
	w.drawDelayCurve()
	if w.haveReport {
		w.drawScopeFromReport()
	} else {
		w.drawScopeFromFrame()
	}
}

func (w *workbench) drawDirection() {
	w.direction.RemoveAll()
	size := fyne.NewSize(640, 210)
	w.direction.Resize(size)
	cx, cy := float32(320), float32(122)
	center := canvas.NewCircle(color.NRGBA{R: 240, G: 196, B: 92, A: 255})
	center.Move(fyne.NewPos(cx-8, cy-8))
	center.Resize(fyne.NewSize(16, 16))
	w.direction.Add(center)
	label := canvas.NewText("FRONT", color.NRGBA{R: 150, G: 170, B: 190, A: 255})
	label.Move(fyne.NewPos(cx-25, 4))
	w.direction.Add(label)
	frame := w.snapshots[w.selected]
	if frame.Result == nil {
		text := canvas.NewText("No estimated parameters for this frame", theme.ForegroundColor())
		text.Move(fyne.NewPos(20, 90))
		w.direction.Add(text)
		w.direction.Refresh()
		return
	}
	for i, channel := range frame.Result.Reconstruction.Channels {
		angle := channel.DirectionDegree * math.Pi / 180
		radius := float32(82)
		x := cx + float32(math.Sin(angle))*radius
		y := cy - float32(math.Cos(angle))*radius
		line := canvas.NewLine(channelColor(i))
		line.StrokeWidth = 2
		line.Position1 = fyne.NewPos(cx, cy)
		line.Position2 = fyne.NewPos(x, y)
		w.direction.Add(line)
		dot := canvas.NewCircle(channelColor(i))
		dot.Move(fyne.NewPos(x-6, y-6))
		dot.Resize(fyne.NewSize(12, 12))
		w.direction.Add(dot)
		text := canvas.NewText(channel.ID, channelColor(i))
		text.Move(fyne.NewPos(x+8, y-7))
		w.direction.Add(text)
	}
	w.direction.Refresh()
}

func (w *workbench) drawEnergyCurve() {
	w.energyPlot.RemoveAll()
	w.energyPlot.Resize(fyne.NewSize(640, 170))
	maxEnergy := 1e-12
	for _, frame := range w.snapshots {
		if frame.Result == nil {
			continue
		}
		for _, ch := range frame.Result.Reconstruction.Channels {
			maxEnergy = math.Max(maxEnergy, ch.EarlyEnergy)
		}
	}
	w.drawChannelCurves(w.energyPlot, func(frame session.FrameSnapshot, channel int) (float64, bool) {
		if frame.Result == nil || channel >= len(frame.Result.Reconstruction.Channels) {
			return 0, false
		}
		return frame.Result.Reconstruction.Channels[channel].EarlyEnergy / maxEnergy, true
	})
	w.energyPlot.Refresh()
}

func (w *workbench) drawDelayCurve() {
	w.delayPlot.RemoveAll()
	w.delayPlot.Resize(fyne.NewSize(640, 170))
	minDelay, maxDelay := math.MaxFloat64, 0.0
	for _, frame := range w.snapshots {
		if frame.Result == nil {
			continue
		}
		for _, ch := range frame.Result.Reconstruction.Channels {
			v := float64(ch.DirectDelay)
			minDelay = math.Min(minDelay, v)
			maxDelay = math.Max(maxDelay, v)
		}
	}
	if minDelay == math.MaxFloat64 {
		minDelay = 0
	}
	span := math.Max(1, maxDelay-minDelay)
	w.drawChannelCurves(w.delayPlot, func(frame session.FrameSnapshot, channel int) (float64, bool) {
		if frame.Result == nil || channel >= len(frame.Result.Reconstruction.Channels) {
			return 0, false
		}
		return (float64(frame.Result.Reconstruction.Channels[channel].DirectDelay) - minDelay) / span, true
	})
	w.delayPlot.Refresh()
}

func (w *workbench) drawChannelCurves(target *fyne.Container, value func(session.FrameSnapshot, int) (float64, bool)) {
	if len(w.snapshots) < 2 {
		return
	}
	channels := len(w.sample.Layout.Channels)
	const width, height, pad = float32(620), float32(150), float32(18)
	for channel := 0; channel < channels; channel++ {
		var previous fyne.Position
		havePrevious := false
		for index, frame := range w.snapshots {
			v, ok := value(frame, channel)
			if !ok {
				havePrevious = false
				continue
			}
			x := pad + float32(index)*width/float32(len(w.snapshots)-1)
			y := height - pad - float32(math.Max(0, math.Min(1, v)))*(height-2*pad)
			if havePrevious {
				line := canvas.NewLine(channelColor(channel))
				line.StrokeWidth = 2
				line.Position1 = previous
				line.Position2 = fyne.NewPos(x, y)
				target.Add(line)
			}
			previous = fyne.NewPos(x, y)
			havePrevious = true
		}
	}
	for channel := 0; channel < channels && channel < len(w.sample.Channels); channel++ {
		text := canvas.NewText(w.sample.Channels[channel].ID, channelColor(channel))
		text.Move(fyne.NewPos(12+float32(channel)*42, 6))
		target.Add(text)
	}
}

func (w *workbench) drawScopeFromReport() {
	sources := [][][]float64{w.report.Realtime.Output, w.report.Resume.Output, w.report.Recompute.Output}
	w.drawScope(sources)
}

func (w *workbench) drawScopeFromFrame() {
	frame := w.snapshots[w.selected]
	if frame.Result == nil {
		w.scopePlot.RemoveAll()
		text := canvas.NewText("No waveform for this frame", theme.ForegroundColor())
		text.Move(fyne.NewPos(20, 90))
		w.scopePlot.Add(text)
		w.scopePlot.Refresh()
		return
	}
	w.drawScope([][][]float64{frame.Result.Render})
}

func (w *workbench) drawScope(sources [][][]float64) {
	w.scopePlot.RemoveAll()
	w.scopePlot.Resize(fyne.NewSize(640, 210))
	if len(sources) == 0 || len(sources[0]) == 0 || len(sources[0][0]) == 0 {
		return
	}
	const width, height, pad = float32(620), float32(185), float32(14)
	colors := []color.Color{
		color.NRGBA{R: 94, G: 176, B: 255, A: 255},
		color.NRGBA{R: 255, G: 177, B: 79, A: 255},
		color.NRGBA{R: 128, G: 226, B: 156, A: 255},
	}
	for sourceIndex, source := range sources {
		if len(source) == 0 {
			continue
		}
		data := source[0]
		if len(data) == 0 {
			continue
		}
		points := 260
		if len(data) < points {
			points = len(data)
		}
		previous := fyne.Position{}
		for point := 0; point < points; point++ {
			start := point * len(data) / points
			end := (point + 1) * len(data) / points
			if end <= start {
				end = start + 1
			}
			peak := 0.0
			for _, v := range data[start:end] {
				if math.Abs(v) > math.Abs(peak) {
					peak = v
				}
			}
			x := pad + float32(point)*width/float32(points-1)
			y := height/2 - float32(math.Max(-1, math.Min(1, peak)))*(height/2-pad)
			if point > 0 {
				line := canvas.NewLine(colors[sourceIndex%len(colors)])
				line.StrokeWidth = float32(1 + sourceIndex)
				line.Position1 = previous
				line.Position2 = fyne.NewPos(x, y)
				w.scopePlot.Add(line)
			}
			previous = fyne.NewPos(x, y)
		}
	}
	legend := canvas.NewText("realtime / resume / recompute", color.NRGBA{R: 170, G: 185, B: 200, A: 255})
	legend.Move(fyne.NewPos(14, 8))
	w.scopePlot.Add(legend)
	w.scopePlot.Refresh()
}

func (w *workbench) tableText(id widget.TableCellID) string {
	if id.Row < 0 || id.Row >= len(w.snapshots) {
		return ""
	}
	frame := w.snapshots[id.Row]
	switch id.Col {
	case 0:
		return strconv.Itoa(frame.Index)
	case 1:
		return string(frame.State)
	case 2:
		if frame.Seq < 0 {
			return "-"
		}
		return strconv.Itoa(frame.Seq)
	case 3:
		return strconv.Itoa(frame.Params.FrameLength)
	case 4:
		return strconv.Itoa(frame.Params.Hop)
	case 5:
		return strconv.Itoa(frame.Params.QuantStep)
	case 6:
		return channelValues(frame, func(ch reconstructView) string { return fmt.Sprintf("%s:%d", ch.ID, ch.DirectDelay) })
	case 7:
		return channelValues(frame, func(ch reconstructView) string { return fmt.Sprintf("%s:%.4g", ch.ID, ch.EarlyEnergy) })
	case 8:
		return channelValues(frame, func(ch reconstructView) string { return fmt.Sprintf("%s:%d", ch.ID, ch.ITDSamples) })
	case 9:
		return channelValues(frame, func(ch reconstructView) string { return fmt.Sprintf("%s:%.2f", ch.ID, ch.DRRDB) })
	case 10:
		if frame.Result == nil {
			return "-"
		}
		b := frame.Result.Reconstruction.Bounds
		return fmt.Sprintf("q%.3g e%.3g t%.3g c%.3g", b.Quantization.Cumulative, b.EnergyTruncation.Cumulative, b.ITD.Cumulative, b.ConvolutionInterp.Cumulative)
	case 11:
		if frame.Error == nil {
			return ""
		}
		return frame.Error.Code
	default:
		return ""
	}
}

type reconstructView struct {
	ID          string
	DirectDelay int
	EarlyEnergy float64
	ITDSamples  int
	DRRDB       float64
}

func channelValues(frame session.FrameSnapshot, value func(reconstructView) string) string {
	if frame.Result == nil {
		return "-"
	}
	parts := make([]string, 0, len(frame.Result.Reconstruction.Channels))
	for _, ch := range frame.Result.Reconstruction.Channels {
		parts = append(parts, value(reconstructView{ID: ch.ID, DirectDelay: ch.DirectDelay, EarlyEnergy: ch.EarlyEnergy, ITDSamples: ch.ITDSamples, DRRDB: ch.DRRDB}))
	}
	return strings.Join(parts, ",")
}

func channelColor(index int) color.Color {
	palette := []color.NRGBA{
		{R: 96, G: 176, B: 255, A: 255},
		{R: 255, G: 177, B: 79, A: 255},
		{R: 128, G: 226, B: 156, A: 255},
		{R: 235, G: 112, B: 128, A: 255},
		{R: 198, G: 154, B: 255, A: 255},
		{R: 102, G: 224, B: 224, A: 255},
	}
	return palette[index%len(palette)]
}

func (w *workbench) showError(err *werr.Error) {
	if err == nil {
		return
	}
	w.displayError = err
	w.status.SetText(err.Error())
	dialog.ShowError(err, w.window)
}

func progressText(progress session.Progress) string {
	return fmt.Sprintf("%d/%d", progress.Committed, progress.Total)
}

func entryInt(entry *widget.Entry, field string) (int, *werr.Error) {
	value, err := strconv.Atoi(strings.TrimSpace(entry.Text))
	if err != nil {
		e := werr.Para("invalid integer for %s: %q", field, entry.Text)
		e.Field = field
		return 0, e
	}
	return value, nil
}

func entryFloat(entry *widget.Entry, field string) (float64, *werr.Error) {
	value, err := strconv.ParseFloat(strings.TrimSpace(entry.Text), 64)
	if err != nil {
		e := werr.Para("invalid number for %s: %q", field, entry.Text)
		e.Field = field
		return 0, e
	}
	return value, nil
}

func parseRouting(value string) (map[string]int, *werr.Error) {
	routing := map[string]int{}
	for _, item := range strings.Split(value, ",") {
		parts := strings.Split(strings.TrimSpace(item), ":")
		if len(parts) != 2 {
			return nil, werr.New(werr.CodeLayout, "invalid route %q", item)
		}
		index, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil, werr.New(werr.CodeLayout, "invalid route index %q", parts[1])
		}
		routing[parts[0]] = index
	}
	return routing, nil
}

func formatRouting(routing map[string]int, channels []string) string {
	parts := make([]string, 0, len(channels))
	for _, id := range channels {
		parts = append(parts, fmt.Sprintf("%s:%d", id, routing[id]))
	}
	return strings.Join(parts, ",")
}

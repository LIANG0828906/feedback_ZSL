package workbench

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"occlusionlab/internal/generator"
	"occlusionlab/internal/geometry"
	"occlusionlab/internal/model"
	"occlusionlab/internal/schema"
	"occlusionlab/internal/store"
	"occlusionlab/internal/streaming"
	"occlusionlab/internal/verify"
)

const AppVersion = "occlusionlab-workbench/1"

type sessionConfig struct {
	Seed       int64 `json:"seed"`
	WorldBits  uint8 `json:"world_frac_bits"`
	BasisBits  uint8 `json:"basis_frac_bits"`
	ScreenBits uint8 `json:"screen_frac_bits"`
	ParamBits  uint8 `json:"param_frac_bits"`
	Raster     int   `json:"raster_resolution"`
	Workers    int   `json:"max_workers"`
	Resident   int   `json:"max_resident"`
	Tolerance  int64 `json:"tolerance_px_q"`
}

type Service struct {
	mu             sync.Mutex
	dataDir        string
	projectDir     string
	checkpointPath string
	sessionPath    string
	project        *model.Project
	params         model.Params
	engine         *streaming.Engine
	checkpoint     *store.CheckpointStore
	selected       int
	paused         bool
	snapshot       SnapshotInfo
	lastValidation *verify.Report
	invalid        map[int]InvalidWindow
	logs           []LogEntry
}

func NewService(dataDir string) *Service {
	return &Service{
		dataDir:  dataDir,
		selected: 0,
		invalid:  map[int]InvalidWindow{},
	}
}

func (s *Service) Bootstrap() (*Dashboard, *model.AppError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.engine != nil {
		return s.dashboardLocked()
	}
	s.projectDir = filepath.Join(s.dataDir, "projects", "current")
	s.sessionPath = filepath.Join(s.dataDir, "current.json")
	s.checkpointPath = filepath.Join(s.dataDir, "checkpoints.sqlite")
	cfg := s.defaultSession()
	if data, err := os.ReadFile(s.sessionPath); err == nil {
		if json.Unmarshal(data, &cfg) != nil {
			cfg = s.defaultSession()
		}
	}
	defaults := s.defaultSession()
	if cfg.WorldBits == 0 {
		cfg.WorldBits = defaults.WorldBits
	}
	if cfg.BasisBits == 0 {
		cfg.BasisBits = defaults.BasisBits
	}
	if cfg.ScreenBits == 0 {
		cfg.ScreenBits = defaults.ScreenBits
	}
	if cfg.ParamBits == 0 {
		cfg.ParamBits = defaults.ParamBits
	}
	if cfg.Raster == 0 {
		cfg.Raster = defaults.Raster
	}
	if cfg.Workers == 0 {
		cfg.Workers = defaults.Workers
	}
	if cfg.Resident == 0 {
		cfg.Resident = defaults.Resident
	}
	if cfg.Tolerance == 0 {
		cfg.Tolerance = defaults.Tolerance
	}
	if err := s.ensureProjectLocked(cfg); err != nil {
		return nil, err
	}
	s.params = model.Params{
		Quantization:      s.project.Meta.Precision,
		RasterResolution:  cfg.Raster,
		MaxWorkers:        cfg.Workers,
		RasterBufferCells: maxInt(cfg.Raster*cfg.Raster, 128*128),
		MaxResident:       cfg.Resident,
		LoadCostBudget:    1000,
		TolerancePxQ:      cfg.Tolerance,
		Version:           1,
	}
	if s.params.TolerancePxQ <= 0 {
		s.params.TolerancePxQ = s.project.Meta.TolerancePxQ
	}
	if err := schema.ValidateParams(s.params); err != nil {
		return nil, err
	}
	if err := s.openEngineLocked(true); err != nil {
		return nil, err
	}
	s.saveSessionLocked()
	s.log("info", fmt.Sprintf("已加载场景 %s；快照 %d 个窗口", s.project.Meta.Title, s.snapshot.Count))
	return s.dashboardLocked()
}

func (s *Service) ensureProjectLocked(cfg sessionConfig) *model.AppError {
	if err := os.MkdirAll(s.dataDir, 0o755); err != nil {
		return model.IOError("data_dir", "无法创建数据目录", err)
	}
	s.projectDir = filepath.Join(s.dataDir, "projects", "current")
	s.sessionPath = filepath.Join(s.dataDir, "current.json")
	s.checkpointPath = filepath.Join(s.dataDir, "checkpoints.sqlite")
	needGenerate := true
	if _, err := os.Stat(filepath.Join(s.projectDir, "manifest.json")); err == nil {
		project, loadErr := schema.LoadProject(s.projectDir)
		if loadErr == nil && project.Meta.Seed == cfg.Seed &&
			project.Meta.Precision.WorldFracBits == cfg.WorldBits &&
			project.Meta.Precision.BasisFracBits == cfg.BasisBits &&
			project.Meta.Precision.ScreenFracBits == cfg.ScreenBits &&
			project.Meta.Precision.ParamFracBits == cfg.ParamBits {
			s.project = project
			needGenerate = false
		}
	}
	if needGenerate {
		genCfg := generator.DefaultConfig(cfg.Seed)
		genCfg.Quantization = model.Quantization{
			WorldFracBits: cfg.WorldBits, BasisFracBits: cfg.BasisBits,
			ScreenFracBits: cfg.ScreenBits, ParamFracBits: cfg.ParamBits,
		}
		genCfg.TolerancePxQ = cfg.Tolerance
		project, genErr := generator.GenerateToDir(s.projectDir, genCfg)
		if genErr != nil {
			return genErr
		}
		s.project = project
		s.log("info", fmt.Sprintf("按种子 %d 生成场景工程", cfg.Seed))
	}
	return nil
}

func (s *Service) openEngineLocked(autoReplay bool) *model.AppError {
	if s.checkpoint != nil {
		s.checkpoint.Close()
	}
	header := store.Header{
		SchemaVersion: model.SchemaVersion,
		StoreVersion:  store.StoreVersion,
		SceneHash:     s.project.ContentHash,
	}
	checkpoint, err := store.Open(s.checkpointPath, header)
	if err != nil {
		return err
	}
	s.checkpoint = checkpoint
	s.snapshot = SnapshotInfo{Path: s.checkpointPath, LastWindow: -1}
	loaded, err := checkpoint.LoadAll()
	if err != nil {
		return err
	}
	if loaded.Invalidated {
		s.snapshot.Invalidated = true
		s.snapshot.Reason = loaded.Reason
		s.log("warn", "检查点整体作废："+loaded.Reason)
		for i := range s.project.World.Cameras {
			s.invalid[i] = InvalidWindow{Index: i, Reason: loaded.Reason, Kind: "version"}
		}
	}
	engine, err := streaming.NewEngine(s.project, s.params, checkpoint)
	if err != nil {
		return err
	}
	if len(loaded.Snapshots) > 0 {
		if err := engine.Restore(loaded.Snapshots); err != nil {
			return err
		}
		s.params = loaded.Snapshots[len(loaded.Snapshots)-1].Params
		s.snapshot.Count = len(loaded.Snapshots)
		s.snapshot.LastWindow = len(loaded.Snapshots) - 1
	}
	s.engine = engine
	if loaded.Invalidated && autoReplay {
		committed, _, _ := engine.Progress()
		for {
			frame, processErr := engine.ProcessNext(context.Background())
			if processErr != nil {
				return processErr
			}
			if frame == nil {
				break
			}
		}
		committed, _, _ = engine.Progress()
		s.snapshot.Invalidated = true
		s.snapshot.Reason = loaded.Reason
		s.snapshot.Count = committed
		s.snapshot.LastWindow = committed - 1
		s.log("warn", fmt.Sprintf("版本不匹配，已全量重放 %d 个窗口", committed))
	}
	if committed, _, _ := engine.Progress(); committed > 0 {
		s.selected = committed - 1
	}
	return nil
}

func (s *Service) Regenerate(opts GenerateOptions) (*Dashboard, *model.AppError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if opts.WorldFracBits == 0 {
		opts.WorldFracBits = 8
	}
	if opts.BasisFracBits == 0 {
		opts.BasisFracBits = 16
	}
	if opts.ScreenFracBits == 0 {
		opts.ScreenFracBits = 8
	}
	if opts.ParamFracBits == 0 {
		opts.ParamFracBits = 16
	}
	if opts.RasterResolution == 0 {
		opts.RasterResolution = 128
	}
	if opts.MaxWorkers == 0 {
		opts.MaxWorkers = 2
	}
	if opts.MaxResident == 0 {
		opts.MaxResident = 15
	}
	if opts.TolerancePx <= 0 {
		opts.TolerancePx = 2
	}
	cfg := sessionConfig{
		Seed: opts.Seed, WorldBits: opts.WorldFracBits, BasisBits: opts.BasisFracBits,
		ScreenBits: opts.ScreenFracBits, ParamBits: opts.ParamFracBits,
		Raster: opts.RasterResolution, Workers: opts.MaxWorkers, Resident: opts.MaxResident,
		Tolerance: int64(opts.TolerancePx * float64(int64(1)<<opts.ScreenFracBits)),
	}
	genCfg := generator.DefaultConfig(cfg.Seed)
	genCfg.Quantization = model.Quantization{
		WorldFracBits: cfg.WorldBits, BasisFracBits: cfg.BasisBits,
		ScreenFracBits: cfg.ScreenBits, ParamFracBits: cfg.ParamBits,
	}
	genCfg.TolerancePxQ = cfg.Tolerance
	project, err := generator.GenerateToDir(s.projectDir, genCfg)
	if err != nil {
		return nil, err
	}
	if s.checkpoint != nil {
		s.checkpoint.Close()
		s.checkpoint = nil
	}
	if err := os.Remove(s.checkpointPath); err != nil && !os.IsNotExist(err) {
		return nil, model.IOError("checkpoint_path", "无法清除旧检查点", err)
	}
	s.project = project
	s.params = model.Params{
		Quantization:     project.Meta.Precision,
		RasterResolution: cfg.Raster, MaxWorkers: cfg.Workers,
		RasterBufferCells: maxInt(cfg.Raster*cfg.Raster, 128*128),
		MaxResident:       cfg.Resident, LoadCostBudget: 1000,
		TolerancePxQ: cfg.Tolerance, Version: 1,
	}
	if err := schema.ValidateParams(s.params); err != nil {
		return nil, err
	}
	s.invalid = map[int]InvalidWindow{}
	s.snapshot = SnapshotInfo{Path: s.checkpointPath, LastWindow: -1}
	s.lastValidation = nil
	s.selected = 0
	s.log("info", fmt.Sprintf("重新生成场景：seed=%d precision=%d/%d/%d/%d raster=%d",
		cfg.Seed, cfg.WorldBits, cfg.BasisBits, cfg.ScreenBits, cfg.ParamBits, cfg.Raster))
	s.saveSessionLocked()
	if err := s.openEngineLocked(false); err != nil {
		return nil, err
	}
	return s.dashboardLocked()
}

func (s *Service) UpdateParams(opts RuntimeOptions) (*Dashboard, *model.AppError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.engine == nil {
		return nil, model.StateError(0, "工作台尚未初始化")
	}
	next := s.params
	if opts.RasterResolution > 0 {
		next.RasterResolution = opts.RasterResolution
	}
	if opts.MaxWorkers > 0 {
		next.MaxWorkers = opts.MaxWorkers
	}
	if opts.RasterBufferCells > 0 {
		next.RasterBufferCells = opts.RasterBufferCells
	}
	if opts.MaxResident > 0 {
		next.MaxResident = opts.MaxResident
	}
	if opts.LoadCostBudget > 0 {
		next.LoadCostBudget = opts.LoadCostBudget
	}
	if opts.TolerancePx > 0 {
		next.TolerancePxQ = int64(opts.TolerancePx * float64(int64(1)<<s.project.Meta.Precision.ScreenFracBits))
	}
	if next.Hash() != s.params.Hash() {
		next.Version++
	}
	if err := s.engine.UpdateUncommittedParams(next); err != nil {
		return nil, err
	}
	s.params = next
	s.saveSessionLocked()
	s.log("info", "运行参数已应用到未提交窗口；已提交窗口保持冻结")
	return s.dashboardLocked()
}

func (s *Service) Step() (*Dashboard, *model.AppError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.engine == nil {
		return nil, model.StateError(0, "工作台尚未初始化")
	}
	frame, err := s.engine.ProcessNext(context.Background())
	if err != nil {
		s.refreshInvalidLocked()
		s.log("error", err.Error())
		return nil, err
	}
	if frame != nil {
		s.selected = frame.Window
		s.snapshot.Count++
		s.snapshot.LastWindow = frame.Window
		if entry, ok := s.invalid[frame.Window]; ok && entry.Kind == "failed" {
			delete(s.invalid, frame.Window)
		}
		s.log("info", fmt.Sprintf("窗口 %d 已提交：可见=%v 不确定=%v", frame.Window, frame.Visible, frame.Uncertain))
	}
	return s.dashboardLocked()
}

func (s *Service) ProcessAll() (*Dashboard, *model.AppError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.engine == nil {
		return nil, model.StateError(0, "工作台尚未初始化")
	}
	for !s.paused {
		frame, err := s.engine.ProcessNext(context.Background())
		if err != nil {
			s.refreshInvalidLocked()
			s.log("error", err.Error())
			return nil, err
		}
		if frame == nil {
			break
		}
		s.selected = frame.Window
		s.snapshot.Count = frame.Window + 1
		s.snapshot.LastWindow = frame.Window
		delete(s.invalid, frame.Window)
	}
	return s.dashboardLocked()
}

func (s *Service) SetPaused(paused bool) (*Dashboard, *model.AppError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paused = paused
	if s.engine == nil {
		return nil, model.StateError(0, "工作台尚未初始化")
	}
	return s.dashboardLocked()
}

func (s *Service) Retry(window int) (*Dashboard, *model.AppError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.engine == nil {
		return nil, model.StateError(0, "工作台尚未初始化")
	}
	if err := s.engine.RetryWindow(window); err != nil {
		return nil, err
	}
	delete(s.invalid, window)
	s.log("info", fmt.Sprintf("窗口 %d 已进入重试待处理状态", window))
	return s.dashboardLocked()
}

func (s *Service) SelectWindow(window int) (*Dashboard, *model.AppError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.engine == nil {
		return nil, model.StateError(0, "工作台尚未初始化")
	}
	if window < 0 || window >= len(s.project.World.Cameras) {
		return nil, model.ParamError("selected_window", "窗口编号越界")
	}
	s.selected = window
	return s.dashboardLocked()
}

func (s *Service) MoveCamera(window int, dx, dy, dz float64) (*Dashboard, *model.AppError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.engine == nil {
		return nil, model.StateError(0, "工作台尚未初始化")
	}
	if window < 0 || window >= len(s.project.World.Cameras) {
		return nil, model.ParamError("window", "窗口编号越界")
	}
	if math.IsNaN(dx) || math.IsNaN(dy) || math.IsNaN(dz) ||
		math.IsInf(dx, 0) || math.IsInf(dy, 0) || math.IsInf(dz, 0) {
		return nil, model.ParamError("camera_delta", "相机位移必须是有限数值")
	}
	bits := s.project.Meta.Precision.WorldFracBits
	scale := float64(int64(1) << bits)
	camera := s.project.World.Cameras[window]
	camera.Position.X += int64(math.Round(dx * scale))
	camera.Position.Y += int64(math.Round(dy * scale))
	camera.Position.Z += int64(math.Round(dz * scale))
	if err := s.engine.UpdateUncommittedCamera(window, camera); err != nil {
		return nil, err
	}
	s.selected = window
	s.log("info", fmt.Sprintf("窗口 %d 相机位置调整为 (%s, %s, %s)",
		window, worldText(camera.Position.X, bits), worldText(camera.Position.Y, bits),
		worldText(camera.Position.Z, bits)))
	return s.dashboardLocked()
}

func (s *Service) Validate() (*verify.Report, *model.AppError) {
	s.mu.Lock()
	project := s.project
	params := s.params
	s.mu.Unlock()
	if project == nil {
		return nil, model.StateError(0, "工作台尚未初始化")
	}
	report, err := verify.Run(context.Background(), project, params, "")
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.lastValidation = report
	if report.Passed {
		s.log("info", "三路径与精度验收通过")
	} else {
		s.log("warn", "三路径或精度验收未通过")
	}
	s.mu.Unlock()
	return report, nil
}

func (s *Service) Reset() (*Dashboard, *model.AppError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.checkpoint != nil {
		s.checkpoint.Close()
		s.checkpoint = nil
	}
	if err := os.Remove(s.checkpointPath); err != nil && !os.IsNotExist(err) {
		return nil, model.IOError("checkpoint_path", "无法清除检查点", err)
	}
	s.invalid = map[int]InvalidWindow{}
	s.snapshot = SnapshotInfo{Path: s.checkpointPath, LastWindow: -1}
	s.lastValidation = nil
	s.selected = 0
	s.log("warn", "检查点已清空，将从窗口 0 重新推进")
	if err := s.openEngineLocked(false); err != nil {
		return nil, err
	}
	return s.dashboardLocked()
}

func (s *Service) Dashboard() (*Dashboard, *model.AppError) {
	s.mu.Lock()
	if s.engine == nil {
		s.mu.Unlock()
		return s.Bootstrap()
	}
	defer s.mu.Unlock()
	return s.dashboardLocked()
}

func (s *Service) dashboardLocked() (*Dashboard, *model.AppError) {
	committed, failed, total := s.engine.Progress()
	dash := &Dashboard{
		AppVersion: AppVersion,
		Project: ProjectSummary{
			Meta: s.project.Meta, ContentHash: s.project.ContentHash,
			World: s.project.World, Truth: s.project.Truth,
		},
		Params:         s.params,
		Progress:       Progress{Committed: committed, Failed: failed, Total: total},
		SelectedWindow: s.selected,
		Paused:         s.paused,
		Snapshot:       s.snapshot,
		Verification:   s.lastValidation,
		Logs:           append([]LogEntry(nil), s.logs...),
		BudgetEvents:   s.engine.BudgetEvents(),
	}
	dash.CanStep = committed+failed < total && failed == 0
	dash.CanRetry = failed > 0
	for i := range s.project.World.Cameras {
		view, err := s.engine.WindowView(i)
		if err != nil {
			return nil, err
		}
		window := WindowSummary{Index: i, Time: s.project.World.Cameras[i].Time, State: view.State, LastError: view.LastError}
		if view.Frame != nil {
			window.Visible = view.Frame.Visible
			window.Occluded = view.Frame.Occluded
			window.Uncertain = view.Frame.Uncertain
			window.Resident = view.Frame.Resident
			window.Loaded = view.Frame.Loaded
			window.Unloaded = view.Frame.Unloaded
			window.CameraOffset = view.Frame.CameraOffset
			window.ParamsVersion = view.Frame.ParamsVersion
			window.Error = view.Frame.Error
			dash.ErrorSeries = append(dash.ErrorSeries, ErrorPoint{
				Window: i, Time: window.Time, Committed: true,
				ProjectionResidualQ:   view.Frame.Error.ProjectionResidualQ,
				ProjectionErrorQ:      view.Frame.Error.ProjectionErrorQ,
				PlaneErrorWorldQ:      view.Frame.Error.FrustumPlaneErrorWorldQ,
				CoverageErrorQ:        view.Frame.Error.CoverageErrorQ,
				DepthErrorWorldQ:      view.Frame.Error.DepthCompareErrorWorldQ,
				CumulativeUpperBoundQ: view.Frame.Error.CumulativeUpperBoundQ,
			})
		}
		if view.State == streaming.StateFailed && view.LastError != nil {
			s.invalid[i] = InvalidWindow{Index: i, Reason: view.LastError.Error(), Kind: "failed"}
		}
		dash.Windows = append(dash.Windows, window)
	}
	for _, invalid := range s.invalid {
		dash.InvalidWindows = append(dash.InvalidWindows, invalid)
	}
	sort.Slice(dash.InvalidWindows, func(i, j int) bool {
		return dash.InvalidWindows[i].Index < dash.InvalidWindows[j].Index
	})
	if s.selected >= 0 && s.selected < total {
		detail, err := s.windowDetailLocked(s.selected)
		if err != nil {
			return nil, err
		}
		dash.Selected = detail
	}
	if len(dash.ErrorSeries) == 0 && dash.Selected != nil && dash.Selected.Preview {
		dash.ErrorSeries = append(dash.ErrorSeries, ErrorPoint{
			Window: s.selected, Time: dash.Selected.Time, Committed: false,
			ProjectionResidualQ:   dash.Selected.Error.ProjectionResidualQ,
			ProjectionErrorQ:      dash.Selected.Error.ProjectionErrorQ,
			PlaneErrorWorldQ:      dash.Selected.Error.FrustumPlaneErrorWorldQ,
			CoverageErrorQ:        dash.Selected.Error.CoverageErrorQ,
			DepthErrorWorldQ:      dash.Selected.Error.DepthCompareErrorWorldQ,
			CumulativeUpperBoundQ: dash.Selected.Error.CumulativeUpperBoundQ,
		})
	}
	return dash, nil
}

func (s *Service) windowDetailLocked(index int) (*WindowDetail, *model.AppError) {
	view, err := s.engine.WindowView(index)
	if err != nil {
		return nil, err
	}
	detail := &WindowDetail{
		Window:    index,
		Time:      s.project.World.Cameras[index].Time,
		State:     view.State,
		Camera:    s.project.World.Cameras[index],
		LastError: view.LastError,
	}
	if view.Frame != nil {
		detail.Frozen = true
		detail.Visible = view.Frame.Visible
		detail.Occluded = view.Frame.Occluded
		detail.Uncertain = view.Frame.Uncertain
		detail.Resident = view.Frame.Resident
		detail.Loaded = view.Frame.Loaded
		detail.Unloaded = view.Frame.Unloaded
		detail.References = view.Frame.References
		detail.Chunks = view.Frame.Chunks
		detail.Error = view.Frame.Error
		detail.ParamsHash = view.Frame.ParamsHash
		detail.ParamsVersion = view.Frame.ParamsVersion
		detail.CameraOffset = view.Frame.CameraOffset
	} else {
		result, evalErr := geometry.EvaluateSample(s.project, s.params, index)
		if evalErr != nil {
			return nil, evalErr
		}
		detail.Preview = true
		detail.Visible = result.Visible
		detail.Occluded = result.Occluded
		detail.Uncertain = result.Uncertain
		detail.Chunks = result.Chunks
		detail.ParamsHash = s.params.Hash()
		detail.ParamsVersion = s.params.Version
		detail.CameraOffset = int64(index + 1)
		for _, verdict := range result.Chunks {
			if verdict.ReferenceOccluder != "" {
				detail.References = append(detail.References, streaming.ChunkReference{
					Chunk: verdict.Chunk, Status: verdict.Status, Occluder: verdict.ReferenceOccluder,
				})
			}
			if verdict.Error.ProjectionResidualQ > detail.Error.ProjectionResidualQ {
				detail.Error.ProjectionResidualQ = verdict.Error.ProjectionResidualQ
			}
			if verdict.Error.ProjectionErrorQ > detail.Error.ProjectionErrorQ {
				detail.Error.ProjectionErrorQ = verdict.Error.ProjectionErrorQ
			}
			if verdict.Error.FrustumPlaneErrorWorldQ > detail.Error.FrustumPlaneErrorWorldQ {
				detail.Error.FrustumPlaneErrorWorldQ = verdict.Error.FrustumPlaneErrorWorldQ
			}
			if verdict.Error.OcclusionCoverageErrorQ > detail.Error.CoverageErrorQ {
				detail.Error.CoverageErrorQ = verdict.Error.OcclusionCoverageErrorQ
			}
			if verdict.Error.DepthCompareErrorWorldQ > detail.Error.DepthCompareErrorWorldQ {
				detail.Error.DepthCompareErrorWorldQ = verdict.Error.DepthCompareErrorWorldQ
			}
			if verdict.Error.VisibilityUpperBoundQ > detail.Error.CumulativeUpperBoundQ {
				detail.Error.CumulativeUpperBoundQ = verdict.Error.VisibilityUpperBoundQ
			}
		}
		if len(detail.Visible) > 0 {
			detail.Resident = append([]int(nil), detail.Visible...)
		}
	}
	if index < len(s.project.Truth.Samples) {
		truth := s.project.Truth.Samples[index]
		detail.Truth = &truth
	}
	return detail, nil
}

func (s *Service) refreshInvalidLocked() {
	for _, window := range s.engine.Windows {
		if window.State == streaming.StateFailed && window.LastError != nil {
			s.invalid[window.Index] = InvalidWindow{
				Index: window.Index, Reason: window.LastError.Error(), Kind: "failed",
			}
		} else if window.State == streaming.StateCommitted {
			if entry, ok := s.invalid[window.Index]; ok && entry.Kind == "failed" {
				delete(s.invalid, window.Index)
			}
		}
	}
}

func (s *Service) defaultSession() sessionConfig {
	return sessionConfig{
		Seed: 1042, WorldBits: 8, BasisBits: 16, ScreenBits: 8, ParamBits: 16,
		Raster: 128, Workers: 2, Resident: 15, Tolerance: 2 << 8,
	}
}

func (s *Service) saveSessionLocked() {
	cfg := sessionConfig{
		Seed:       s.project.Meta.Seed,
		WorldBits:  s.project.Meta.Precision.WorldFracBits,
		BasisBits:  s.project.Meta.Precision.BasisFracBits,
		ScreenBits: s.project.Meta.Precision.ScreenFracBits,
		ParamBits:  s.project.Meta.Precision.ParamFracBits,
		Raster:     s.params.RasterResolution,
		Workers:    s.params.MaxWorkers,
		Resident:   s.params.MaxResident,
		Tolerance:  s.params.TolerancePxQ,
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(s.dataDir, 0o755)
	_ = os.WriteFile(s.sessionPath, append(data, '\n'), 0o644)
}

func (s *Service) log(level, message string) {
	s.logs = append(s.logs, LogEntry{
		At: time.Now().UTC().Format(time.RFC3339Nano), Level: level, Message: message,
	})
	if len(s.logs) > 100 {
		s.logs = s.logs[len(s.logs)-100:]
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func worldText(value int64, bits uint8) string {
	return fmt.Sprintf("%.3f", float64(value)/float64(int64(1)<<bits))
}

func (s *Service) DataDir() string { return s.dataDir }

func (s *Service) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.checkpoint != nil {
		s.checkpoint.Close()
		s.checkpoint = nil
	}
}

func (s *Service) ProjectDir() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.projectDir
}

func (s *Service) ExportProject() (string, *model.AppError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.project == nil {
		return "", model.StateError(0, "工作台尚未初始化")
	}
	if err := schema.WriteProject(s.projectDir, s.project); err != nil {
		return "", err
	}
	return s.projectDir, nil
}

func (s *Service) StatusText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.engine == nil {
		return "not-ready"
	}
	committed, failed, total := s.engine.Progress()
	return fmt.Sprintf("%d/%d committed, %d failed", committed, total, failed)
}

func SanitizePath(path string) string {
	return strings.TrimSpace(filepath.Clean(path))
}

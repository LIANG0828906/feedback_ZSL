package web

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"maskpipe/internal/model"
	"maskpipe/internal/pipeline"
	"maskpipe/internal/sample"
	"maskpipe/internal/store"
	"maskpipe/internal/verify"
)

type Manager struct {
	cfg    Config
	loaded *sample.LoadedScene
	store  *store.Store

	mu      sync.Mutex
	service *pipeline.Service
	control *pipeline.Control
	result  *pipeline.Result
	running bool
	lastErr *model.Error
}

func NewManager(cfg Config) (*Manager, error) {
	if cfg.Loaded == nil {
		loaded, err := sample.LoadScene(cfg.SampleDir, cfg.SceneID)
		if err != nil {
			return nil, err
		}
		cfg.Loaded = loaded
	}
	if cfg.DBPath == "" {
		cfg.DBPath = filepath.Join("reports", "web-diagnostic.db")
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return nil, err
	}
	control := pipeline.NewControl()
	svc, err := pipeline.NewService(cfg.Loaded, st, nil, control)
	if err != nil {
		st.Close()
		return nil, err
	}
	return &Manager{cfg: cfg, loaded: cfg.Loaded, store: st, service: svc, control: control}, nil
}

func (m *Manager) Close() { _ = m.store.Close() }

func (m *Manager) StartRun(mode pipeline.RunMode) error {
	return m.StartRunWithParams(mode, nil)
}

func (m *Manager) StartRunWithParams(mode pipeline.RunMode, params *model.ParameterSet) error {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return model.NewError("E_STATE", "run", "a run is already active")
	}
	m.running = true
	m.lastErr = nil
	control := pipeline.NewControl()
	m.control = control
	svc, err := pipeline.NewService(m.loaded, m.store, params, control)
	if err != nil {
		m.running = false
		m.mu.Unlock()
		return err
	}
	m.service = svc
	m.mu.Unlock()

	go func() {
		result, runErr := svc.Run(context.Background(), mode, nil)
		m.mu.Lock()
		m.running = false
		if result != nil {
			m.result = result
		}
		if runErr != nil {
			if pe, ok := model.AsError(runErr); ok {
				m.lastErr = pe
			} else {
				m.lastErr = model.NewError("E_INTERNAL", "run", "%v", runErr)
			}
		} else {
			m.lastErr = nil
		}
		m.mu.Unlock()
	}()
	return nil
}

func (m *Manager) Pause()  { m.control.Pause() }
func (m *Manager) Resume() { m.control.Resume() }
func (m *Manager) Cancel() { m.control.Cancel() }

func (m *Manager) RetryChunk(x, y int, stage model.Stage) error {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return model.NewError("E_STATE", "run", "cannot retry while run is active")
	}
	svc := m.service
	m.mu.Unlock()
	if svc == nil {
		return m.StartRun(pipeline.ModeResume)
	}
	key := model.ChunkKey{ChunkCoord: model.ChunkCoord{X: x, Y: y}, Stage: stage}
	if err := svc.Machine.Transition(key, model.StatusPending, "", nil); err != nil {
		return err
	}
	return m.StartRunWithService(pipeline.ModeResume, svc)
}

func (m *Manager) StartRunWithService(mode pipeline.RunMode, svc *pipeline.Service) error {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return model.NewError("E_STATE", "run", "a run is already active")
	}
	m.running = true
	m.service = svc
	m.control = svc.Control
	m.mu.Unlock()
	go func() {
		result, runErr := svc.Run(context.Background(), mode, nil)
		m.mu.Lock()
		m.running = false
		if result != nil {
			m.result = result
		}
		if runErr != nil {
			if pe, ok := model.AsError(runErr); ok {
				m.lastErr = pe
			} else {
				m.lastErr = model.NewError("E_INTERNAL", "run", "%v", runErr)
			}
		}
		m.mu.Unlock()
	}()
	return nil
}

type StateResponse struct {
	OK           bool                    `json:"ok"`
	Running      bool                    `json:"running"`
	Scene        model.SceneManifest     `json:"scene"`
	DBPath       string                  `json:"db_path"`
	SnapshotID   string                  `json:"snapshot_id"`
	Progress     *pipeline.ProgressEvent `json:"progress,omitempty"`
	LiveStates   []pipeline.StageState   `json:"live_states,omitempty"`
	Result       *pipeline.Result        `json:"result,omitempty"`
	LastError    *model.Error            `json:"last_error,omitempty"`
	Verification *verify.Report          `json:"verification,omitempty"`
}

func (m *Manager) State(ctx context.Context) StateResponse {
	m.mu.Lock()
	running := m.running
	result := m.result
	lastErr := m.lastErr
	snapshotID := ""
	service := m.service
	if service != nil {
		snapshotID = service.SnapshotID()
	}
	m.mu.Unlock()

	resp := StateResponse{
		OK: true, Running: running, Scene: m.loaded.Scene, DBPath: m.cfg.DBPath,
		SnapshotID: snapshotID, Result: result, LastError: lastErr,
	}
	if service != nil {
		committed, total := service.Machine.Progress(len(service.Chunks))
		resp.Progress = &pipeline.ProgressEvent{Committed: committed, Total: total}
		resp.LiveStates = service.Machine.Snapshot()
	}
	if snapshotID != "" && !running {
		report, err := verify.Run(ctx, m.cfg.DBPath, snapshotID, &m.loaded.Scene)
		if err == nil {
			resp.Verification = report
		}
	}
	return resp
}

func (m *Manager) StageRecord(ctx context.Context, x, y int, stage model.Stage) (store.StageRecord, error) {
	id := m.service.SnapshotID()
	return m.store.GetStage(ctx, id, x, y, stage)
}

func (m *Manager) ExportReport(path string) error {
	state := m.State(context.Background())
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

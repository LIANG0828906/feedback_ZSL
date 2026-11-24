// Package server 提供本地 HTTP 工作台：会话管理、增量标注、
// 暂停/取消/重试、三路径比对与导出，全部离线运行。
package server

import (
	"context"
	"os"
	"sync"

	"audiobench/internal/dsp"
	"audiobench/internal/ingest"
	"audiobench/internal/model"
	"audiobench/internal/pipeline"
	"audiobench/internal/window"
)

// Session 单个素材的工作台会话。
type Session struct {
	mu           sync.Mutex
	Material     *model.Material
	Samples      []float64
	Hash         string
	Params       model.Params
	WindowFrames int
	Limits       pipeline.Limits
	DBPath       string
	DataDir      string
	Engine       *pipeline.Engine
	Plan         *window.Plan
	Running      bool
	CancelCtx    context.CancelFunc
	LastAnn      *pipeline.Annotation
}

// Manager 管理全部素材会话。
type Manager struct {
	mu       sync.Mutex
	sessions map[string]*Session
	dataDir  string
	varDir   string
}

func NewManager(dataDir, varDir string) *Manager {
	if err := os.MkdirAll(varDir, 0o755); err != nil {
		panic("无法创建持久化目录 " + varDir + ": " + err.Error())
	}
	return &Manager{sessions: map[string]*Session{}, dataDir: dataDir, varDir: varDir}
}

// OpenSession 加载素材并建立会话（幂等）。
func (m *Manager) OpenSession(descPath string) (*Session, error) {
	mat, err := ingest.LoadMaterial(descPath)
	if err != nil {
		return nil, err
	}
	samples, err := ingest.ReadPCM(mat.PCMFile)
	if err != nil {
		return nil, err
	}
	hash, err := ingest.ContentHash(mat.PCMFile)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[mat.ID]; ok {
		return s, nil
	}
	s := &Session{
		Material: mat, Samples: samples, Hash: hash,
		Params:       model.DefaultParams(),
		WindowFrames: 8,
		Limits:       pipeline.DefaultLimits(),
		DBPath:       m.varDir + "/checkpoint_" + mat.ID + ".db",
		DataDir:      m.dataDir,
	}
	m.sessions[mat.ID] = s
	return s, nil
}

func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	return s, ok
}

func (m *Manager) List() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	return ids
}

// runCfg 在锁外组装一次运行配置。
func (s *Session) runCfg() pipeline.RunConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return pipeline.RunConfig{
		Material: s.Material, Samples: s.Samples, Params: s.Params,
		ContentHash: s.Hash, WindowFrames: s.WindowFrames,
		Limits: s.Limits, DBPath: s.DBPath,
	}
}

// FrameCount 便捷访问。
func (s *Session) FrameCount() int {
	return dsp.FrameCount(len(s.Samples), s.Params.FrameLength, s.Params.FrameShift)
}

package main

import (
	"context"
	"path/filepath"

	"spatialir/pkg/workbench"
)

// App 是 Wails 暴露给前端的绑定，方法与内置 HTTP 面板的 JSON API 同构。
type App struct {
	ctx      context.Context
	dataDir  string
	sessions map[string]*workbench.Session
}

func NewApp(dataDir string) *App {
	return &App{dataDir: dataDir, sessions: map[string]*workbench.Session{}}
}

func (a *App) startup(ctx context.Context) { a.ctx = ctx }

// GenerateSamples 调用本地确定性生成器产出内置样本集。
func (a *App) GenerateSamples() ([]string, error) {
	return workbench.GenerateBuiltin(a.dataDir)
}

// RunPipeline 在桌面端执行 realtime/resume/replay 三条路径之一。
func (a *App) RunPipeline(sample string, params workbench.FrameParams, mode string, pause int) (*workbench.Result, error) {
	sess, ok := a.sessions[sample]
	if !ok {
		ds, err := workbench.LoadSample(a.dataDir, sample)
		if err != nil {
			return nil, err
		}
		dbPath := filepath.Join(a.dataDir, sample, sample+"_desktop.db")
		sess, err = workbench.OpenSession(ds, params, dbPath)
		if err != nil {
			return nil, err
		}
		a.sessions[sample] = sess
	}
	return sess.Run(a.ctx, mode, pause, params)
}

// RetryFailed 把失败帧重置为待估计，已提交帧保持冻结。
func (a *App) RetryFailed(sample string, frames []int) error {
	sess, ok := a.sessions[sample]
	if !ok {
		return workbench.NewError(workbench.EState, "sample not initialized")
	}
	return sess.RetryFailed(frames...)
}

// Shutdown 关闭全部检查点连接。
func (a *App) Shutdown() {
	for _, s := range a.sessions {
		_ = s.Close()
	}
}

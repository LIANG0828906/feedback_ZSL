// Package workbench 是工作台对外公开的稳定 API，CLI、内置面板与 Wails 桌面壳共用。
package workbench

import (
	"context"
	"path/filepath"

	"spatialir/internal/analyzer"
	"spatialir/internal/checkpoint"
	"spatialir/internal/compare"
	"spatialir/internal/generator"
	"spatialir/internal/loader"
	"spatialir/internal/model"
	"spatialir/internal/pipeline"
)

// Re-export 业务类型与错误码，外部调用方无需依赖 internal 包。
type (
	FrameParams   = model.FrameParams
	Manifest      = model.Manifest
	RunReport     = model.RunReport
	Divergence    = model.Divergence
	FaultSpec     = model.FaultSpec
	FrameSnapshot = model.FrameSnapshot
	Result        = pipeline.Result
)

type Frame = analyzer.Frame

const (
	EMeta    = model.CodeMeta
	EPara    = model.CodePara
	EBound   = model.CodeBound
	EState   = model.CodeState
	EChan    = model.CodeChan
	EOrder   = model.CodeOrder
	EClip    = model.CodeClip
	EFrame   = model.CodeFrame
	EDiverge = model.CodeDiverge
)

// NewError 构造业务错误，错误码取本包常量。
func NewError(code model.ErrorCode, reason string) error {
	return model.NewError(code, reason)
}

// GenerateBuiltin 在 dataDir 下产出全部内置确定性样本集。
func GenerateBuiltin(dataDir string) ([]string, error) {
	names := make([]string, 0)
	for _, spec := range generator.BuiltinSpecs() {
		if _, _, _, err := generator.Generate(spec, filepath.Join(dataDir, spec.Name)); err != nil {
			return nil, err
		}
		names = append(names, spec.Name)
	}
	return names, nil
}

// LoadSample 加载单个样本集并校验元数据。
func LoadSample(dataDir, sample string) (*loader.Dataset, error) {
	dir := filepath.Join(dataDir, sample)
	return loader.Load(dir, sample+"_manifest.json")
}

// Validate 校验分帧与渲染参数。
func Validate(params FrameParams, length int) error {
	return loader.ValidateParams(params, length)
}

// Session 封装一个样本的引擎与检查点连接。
type Session struct {
	ds    *loader.Dataset
	store *checkpoint.Store
	eng   *pipeline.Engine
}

// OpenSession 打开（或创建）样本会话。
func OpenSession(ds *loader.Dataset, params FrameParams, dbPath string) (*Session, error) {
	if err := loader.ValidateParams(params, ds.Manifest.LengthSamples); err != nil {
		return nil, err
	}
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		return nil, err
	}
	return &Session{ds: ds, store: store, eng: pipeline.NewEngine(ds, params)}, nil
}

func (s *Session) Close() error { return s.store.Close() }

// Run 执行 realtime / resume / replay 路径。
func (s *Session) Run(ctx context.Context, mode string, pauseAt int, params FrameParams) (*Result, error) {
	return s.eng.Run(ctx, pipeline.Options{
		Dataset: s.ds, Params: params, Store: s.store,
		Mode: pipeline.RunMode(mode), PauseAt: pauseAt,
	})
}

// RetryFailed 重置失败帧为待估计；已提交帧保持冻结。
func (s *Session) RetryFailed(frames ...int) error { return s.eng.RetryFailed(frames...) }

// CompareReports 比对两份报告并返回首个差异（nil 表示逐样本一致）。
func CompareReports(a, b *RunReport) *Divergence {
	frames := make([]analyzer.Frame, 0, len(a.FrameReports))
	for _, f := range a.FrameReports {
		frames = append(frames, analyzer.Frame{Index: f.Index, Start: f.Start, End: f.End})
	}
	return compare.Compare(a, b, frames)
}

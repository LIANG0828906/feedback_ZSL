package pipeline

import (
	"context"
	"os"
	"path/filepath"

	"mcapture/internal/session"
	"mcapture/internal/store"
)

// RunPath 在给定检查点路径上执行一种处理模式。
// full：忽略/清空旧检查点，全量重算。
// resume：打开既有检查点，按版本与哈希恢复，仅重算未提交分段。
// realtime：语义上按分段流式推进；为保证逐样本一致，复用同一确定性引擎，
// 分段按计划顺序提交，检查点行为与 full 相同。
func RunPath(ctx context.Context, sess *session.Session, cfg AlgoConfig,
	dbPath string, mode Mode, workers int) (*Result, *store.Store, error) {
	if mode == ModeFull || mode == ModeRealtime {
		// 运行前清空旧检查点，避免版本不匹配干扰。
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, nil, err
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return nil, nil, err
	}
	if err := st.PutMeta("session_fingerprint", sess.Manifest.Fingerprint()); err != nil {
		st.Close()
		return nil, nil, err
	}
	if err := st.PutMeta("param_version", cfg.ParamVersion); err != nil {
		st.Close()
		return nil, nil, err
	}
	if err := st.PutMeta("algo_version", cfg.AlgoVersion); err != nil {
		st.Close()
		return nil, nil, err
	}
	eng, err := NewEngine(sess, cfg, st)
	if err != nil {
		st.Close()
		return nil, nil, err
	}
	if err := eng.Run(ctx, mode, workers); err != nil {
		st.Close()
		return nil, nil, err
	}
	res := eng.Collect(mode)
	return res, st, nil
}

// NewRunnableEngine 准备一个绑定检查点的引擎，但不执行处理，
// 供需要长期保留引擎实例（如 Web 端逐段重试）的调用方使用。
func NewRunnableEngine(sess *session.Session, cfg AlgoConfig, st *store.Store) (*Engine, error) {
	return NewEngine(sess, cfg, st)
}

// RunEngine 在给定引擎上执行一种模式并收集结果。
func RunEngine(ctx context.Context, eng *Engine, mode Mode, workers int) (*Result, error) {
	if err := eng.Run(ctx, mode, workers); err != nil {
		return nil, err
	}
	return eng.Collect(mode), nil
}

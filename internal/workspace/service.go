package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"atelier/internal/apperr"
	"atelier/internal/checkpoint"
	"atelier/internal/engine"
	"atelier/internal/model"
	"atelier/internal/sample"
	"atelier/internal/scenarioio"
)

type APIResponse struct {
	OK      bool          `json:"ok"`
	Code    string        `json:"code,omitempty"`
	Message string        `json:"message,omitempty"`
	Data    any           `json:"data,omitempty"`
	Error   *apperr.Error `json:"error,omitempty"`
}

type RegionOption struct {
	Seed        int64  `json:"seed"`
	RegionID    string `json:"regionId"`
	Name        string `json:"name"`
	Profile     string `json:"profile"`
	Description string `json:"description"`
}

type UpdateZoneRequest struct {
	ZoneID       string  `json:"zoneId"`
	DensityScale float64 `json:"densityScale"`
	RarityShift  float64 `json:"rarityShift"`
}

type MoveExclusionRequest struct {
	ExclusionID string  `json:"exclusionId"`
	DX          float64 `json:"dx"`
	DY          float64 `json:"dy"`
}

type BoundaryRequest struct {
	ZoneID  string        `json:"zoneId"`
	Polygon model.Polygon `json:"polygon"`
}

type ExportPayload struct {
	Filename  string          `json:"filename"`
	JSON      string          `json:"json"`
	Generated string          `json:"generated"`
	Snapshot  engine.Snapshot `json:"snapshot"`
}

type ComparePayload struct {
	Result   engine.ComparisonResult `json:"result"`
	Snapshot engine.Snapshot         `json:"snapshot"`
}

type Service struct {
	mu       sync.Mutex
	cancelMu sync.Mutex
	cancel   context.CancelFunc
	store    *checkpoint.Store
	engine   *engine.Engine
	dbPath   string
	lastSeed int64
}

func New(dbPath string) (*Service, error) {
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("%s: open checkpoint store: %w", apperr.CodeIO, err)
	}
	return &Service{store: store, dbPath: dbPath, lastSeed: sample.DefaultSeed}, nil
}

func (s *Service) Close() error {
	if s.store != nil {
		return s.store.Close()
	}
	return nil
}

func Catalog() []RegionOption {
	return []RegionOption{
		{Seed: 2042, RegionID: "frontier-basin", Name: "边境盆地", Profile: "基准样本", Description: "四地带、四地形、三处禁采区；固定种子 2042。"},
		{Seed: 7183, RegionID: "ember-rift", Name: "赤谷裂原", Profile: "横向裂谷", Description: "三地带横向拓扑、独立地形带与三处禁采区；固定种子 7183。"},
		{Seed: 9911, RegionID: "tidal-reach", Name: "潮汐边缘", Profile: "对角潮区", Description: "四地带对角拓扑、潮间地形与三处禁采区；固定种子 9911。"},
	}
}

func (s *Service) Bootstrap(ctx context.Context) APIResponse {
	return s.guard(ctx, func(ctx context.Context) (any, error) {
		return s.loadSeedLocked(ctx, sample.DefaultSeed)
	})
}

func (s *Service) LoadRegion(ctx context.Context, seed int64) APIResponse {
	return s.guard(ctx, func(ctx context.Context) (any, error) {
		valid := false
		for _, option := range Catalog() {
			if option.Seed == seed {
				valid = true
				break
			}
		}
		if !valid {
			return nil, apperr.Schema("unknown region seed %d", seed)
		}
		return s.loadSeedLocked(ctx, seed)
	})
}

func (s *Service) LoadScenarioJSON(ctx context.Context, payload string) APIResponse {
	return s.guard(ctx, func(ctx context.Context) (any, error) {
		scenario, err := scenarioio.Decode([]byte(payload))
		if err != nil {
			return nil, err
		}
		eng, err := engine.New(scenario)
		if err != nil {
			return nil, err
		}
		s.engine = eng
		s.lastSeed = scenario.Seed
		return s.openWithRecoveryLocked(ctx, eng)
	})
}

func (s *Service) Snapshot() APIResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.engine == nil {
		return failure(apperr.Schema("workspace has not been bootstrapped"))
	}
	return success(s.engine.Snapshot())
}

func (s *Service) UpdateZone(ctx context.Context, req UpdateZoneRequest) APIResponse {
	return s.guard(ctx, func(ctx context.Context) (any, error) {
		if s.engine == nil {
			return nil, apperr.Schema("workspace has not been bootstrapped")
		}
		return s.engine.UpdateZoneParams(ctx, req.ZoneID, model.ZoneParams{
			DensityScale: req.DensityScale,
			RarityShift:  req.RarityShift,
		})
	})
}

func (s *Service) MoveExclusion(ctx context.Context, req MoveExclusionRequest) APIResponse {
	return s.guard(ctx, func(ctx context.Context) (any, error) {
		if s.engine == nil {
			return nil, apperr.Schema("workspace has not been bootstrapped")
		}
		return s.engine.MoveExclusion(ctx, req.ExclusionID, req.DX, req.DY)
	})
}

func (s *Service) UpdateZoneBoundary(ctx context.Context, req BoundaryRequest) APIResponse {
	return s.guard(ctx, func(ctx context.Context) (any, error) {
		if s.engine == nil {
			return nil, apperr.Schema("workspace has not been bootstrapped")
		}
		return s.engine.UpdateZoneBoundary(ctx, req.ZoneID, req.Polygon)
	})
}

func (s *Service) RetryZone(ctx context.Context, zoneID string) APIResponse {
	return s.guard(ctx, func(ctx context.Context) (any, error) {
		if s.engine == nil {
			return nil, apperr.Schema("workspace has not been bootstrapped")
		}
		return s.engine.RetryZone(ctx, zoneID)
	})
}

func (s *Service) FullRebuild(ctx context.Context) APIResponse {
	return s.guard(ctx, func(ctx context.Context) (any, error) {
		if s.engine == nil {
			return nil, apperr.Schema("workspace has not been bootstrapped")
		}
		return s.engine.FullRebuild(ctx)
	})
}

func (s *Service) Compare(ctx context.Context) APIResponse {
	return s.guard(ctx, func(ctx context.Context) (any, error) {
		if s.engine == nil {
			return nil, apperr.Schema("workspace has not been bootstrapped")
		}
		result, snapshot, err := s.engine.CompareWithFull(ctx)
		if err != nil {
			var codeErr *apperr.Error
			if errors.As(err, &codeErr) && codeErr.Code == apperr.CodeDiff {
				return ComparePayload{Result: result, Snapshot: snapshot}, err
			}
			return nil, err
		}
		current := s.engine.Snapshot()
		return ComparePayload{Result: result, Snapshot: current}, nil
	})
}

func (s *Service) Cancel() APIResponse {
	s.cancelMu.Lock()
	cancel := s.cancel
	s.cancelMu.Unlock()
	if cancel != nil {
		cancel()
		return success(map[string]any{"cancelled": true, "source": checkpoint.SourceActiveCancel})
	}
	return success(map[string]any{"cancelled": false, "source": checkpoint.SourceNone})
}

func (s *Service) Export(ctx context.Context) APIResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.engine == nil {
		return failure(apperr.Schema("workspace has not been bootstrapped"))
	}
	snap := s.engine.Snapshot()
	now := time.Now().UTC()
	name := fmt.Sprintf("atelier-export-%s-%d.json", snap.RegionID, now.Unix())
	b, err := json.MarshalIndent(struct {
		Generated string          `json:"generated"`
		Snapshot  engine.Snapshot `json:"snapshot"`
	}{Generated: now.Format(time.RFC3339), Snapshot: snap}, "", "  ")
	if err != nil {
		return failure(apperr.Schema("encode export: %v", err))
	}
	return success(ExportPayload{
		Filename: name, JSON: string(b), Generated: now.Format(time.RFC3339), Snapshot: snap,
	})
}

func (s *Service) SaveGeneratedSample(ctx context.Context, seed int64) APIResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	scenario := sample.Generate(seed)
	cwd, err := os.Getwd()
	if err != nil {
		return failure(err)
	}
	path := filepath.Join(cwd, "samples", fmt.Sprintf("scenario-seed-%d.json", seed))
	if err := scenarioio.Write(path, scenario); err != nil {
		return failure(err)
	}
	return success(map[string]any{"path": path, "seed": seed, "truthPoints": len(scenario.Truth.Points)})
}

func (s *Service) loadSeedLocked(ctx context.Context, seed int64) (any, error) {
	scenario := sample.Generate(seed)
	eng, err := engine.New(scenario)
	if err != nil {
		return nil, err
	}
	s.engine = eng
	s.lastSeed = seed
	return s.openWithRecoveryLocked(ctx, eng)
}

func (s *Service) openWithRecoveryLocked(ctx context.Context, eng *engine.Engine) (any, error) {
	eng.SetCheckpointSink(s.store)
	req := checkpoint.VerifyRequest{
		ExpectedScenarioHash:  eng.ScenarioHash(),
		ExpectedAlgorithm:     model.AlgorithmVersion,
		ExpectedParamsVersion: eng.ExpectedParamsVersions(),
		ExpectedZoneOrder:     eng.ZoneOrder(),
	}
	recovery, err := s.store.LoadLatest(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("%s: load checkpoint: %w", apperr.CodeIO, err)
	}
	report := engine.RecoveryReport{
		Source:             recovery.RecoverySource,
		SnapshotStatus:     recovery.Status,
		Discarded:          recovery.Discarded,
		DiscardReason:      recovery.DiscardReason,
		RecoveredFromStore: recovery.Valid,
	}
	for _, zone := range recovery.Zones {
		report.ResumedZones = append(report.ResumedZones, zone.ZoneID)
	}
	if recovery.Valid {
		snap, err := eng.ResumeFromCheckpoint(ctx, recovery.Zones, report)
		if err == nil {
			return snap, nil
		}
		report.Discarded = true
		report.DiscardReason = err.Error()
	}
	report.FullRebuild = true
	eng.SetRecovery(report)
	return eng.FullRebuild(ctx)
}

func (s *Service) guard(parent context.Context, fn func(context.Context) (any, error)) APIResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := context.WithCancel(parent)
	s.cancelMu.Lock()
	s.cancel = cancel
	s.cancelMu.Unlock()
	defer func() {
		s.cancelMu.Lock()
		s.cancel = nil
		s.cancelMu.Unlock()
		cancel()
	}()
	data, err := fn(ctx)
	if err != nil {
		var ae *apperr.Error
		if errors.As(err, &ae) {
			resp := failure(ae)
			if data != nil {
				resp.Data = data
			}
			return resp
		}
		if errors.Is(err, context.Canceled) {
			ae = &apperr.Error{Code: apperr.CodeCancel, Message: "operation cancelled; committed zone checkpoints are retained"}
			resp := failure(ae)
			if data != nil {
				resp.Data = data
			}
			return resp
		}
		return failure(&apperr.Error{Code: apperr.CodeIO, Message: err.Error()})
	}
	return success(data)
}

func success(data any) APIResponse {
	return APIResponse{OK: true, Data: data}
}

func failure(err error) APIResponse {
	var ae *apperr.Error
	if !errors.As(err, &ae) {
		ae = &apperr.Error{Code: apperr.CodeIO, Message: err.Error()}
	}
	return APIResponse{OK: false, Code: ae.Code, Message: ae.Message, Error: ae}
}

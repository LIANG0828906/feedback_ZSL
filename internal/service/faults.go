package service

import (
	"context"
	"math"

	"lip-pipeline-prototype/internal/checkpoint"
	"lip-pipeline-prototype/internal/convert"
	"lip-pipeline-prototype/internal/domain"
	"lip-pipeline-prototype/internal/pipeline"
	"lip-pipeline-prototype/internal/schema"
)

func (s *Service) Fault(kind string) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.order) == 0 {
		return &domain.Error{Code: domain.CodeNotFound, Message: "no sample shards"}
	}
	shardID := s.order[0]
	_, original, err := s.findShard(shardID)
	if err != nil {
		return err
	}
	shard := cloneShardForFault(*original)
	params := s.project.Params

	switch kind {
	case "E_ORDER", "order":
		if len(shard.Phonemes) < 2 {
			return &domain.Error{Code: domain.CodeStage, Message: "sample needs at least two phonemes"}
		}
		shard.Phonemes[1].StartSample = shard.Phonemes[0].EndSample - 1
		_, err := pipeline.BuildShard(shard, params, "fault-hash", domain.SchemaV3)
		return err
	case "E_DECODE", "decode":
		shard.TotalSamples += 960
		_, err := pipeline.BuildShard(shard, params, "fault-hash", domain.SchemaV3)
		return err
	case "E_VAL", "val":
		params.TransitionTolerance = -1
		_, err := pipeline.BuildShard(shard, params, "fault-hash", domain.SchemaV3)
		return err
	case "E_STAGE", "stage":
		shard.InjectFailure = true
		_, err := pipeline.BuildShard(shard, params, "fault-hash", domain.SchemaV3)
		return err
	case "E_LIP", "lip":
		params.TransitionTolerance = 1
		_, err := pipeline.BuildShard(shard, params, "fault-hash", domain.SchemaV3)
		return err
	case "E_OVF", "ovf":
		_, err := convert.QuantizeFrame(math.MaxInt64, params.FPS, params.SampleRate, "fault.overflow_sample")
		return err
	case "E_SCHEMA", "schema":
		_, err := schema.LoadDocument([]byte(`{"schema":"lip.project/v9","project_id":"fault"}`))
		return err
	case "E_STATE", "state":
		m := checkpoint.NewMachine("fault-state-shard", 0)
		return m.Transition(checkpoint.StateCommitted, "illegal acceptance probe")
	case "E_DIVERGE", "diverge":
		preview, err := pipeline.BuildPreview(shard, params)
		if err != nil {
			return err
		}
		exact, err := pipeline.BuildShard(shard, params, "fault-hash", domain.SchemaV3)
		if err != nil {
			return err
		}
		if len(preview.SampledFrames) > 2 {
			preview.SampledFrames[2].Mouth = domain.MouthVowel
			preview.SampledFrames[2].Kind = domain.FrameStable
		}
		return pipeline.ComparePreviewExact(preview, exact)
	default:
		return &domain.Error{Code: domain.CodeNotFound, Message: "unknown fault kind: " + kind}
	}
}

func cloneShardForFault(shard domain.Shard) domain.Shard {
	phonemes := append([]domain.Phoneme(nil), shard.Phonemes...)
	shard.Phonemes = phonemes
	return shard
}

func (s *Service) CorruptCheckpoint(ctx context.Context, shardID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, _, err := s.findShard(shardID); err != nil {
		return err
	}
	return s.store.CorruptVersionsForTest(ctx, shardID)
}

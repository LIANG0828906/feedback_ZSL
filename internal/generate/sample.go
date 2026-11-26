package generate

import (
	"fmt"

	"lip-pipeline-prototype/internal/domain"
	"lip-pipeline-prototype/internal/pipeline"
	"lip-pipeline-prototype/internal/schema"
)

type Sample struct {
	Project      *domain.Project                               `json:"project"`
	Documents    map[domain.SchemaVersion][]byte               `json:"-"`
	Loaded       map[domain.SchemaVersion]*schema.LoadResult   `json:"-"`
	Exports      map[domain.SchemaVersion]*schema.ExportResult `json:"-"`
	TruthByShard map[string]*domain.ShardResult                `json:"truth_by_shard"`
	TruthOrder   []string                                      `json:"truth_order"`
}

func BuildSample() (*Sample, error) {
	project := Project()
	versions := []domain.SchemaVersion{domain.SchemaV1, domain.SchemaV2, domain.SchemaV3}
	sample := &Sample{
		Project:      project,
		Documents:    map[domain.SchemaVersion][]byte{},
		Loaded:       map[domain.SchemaVersion]*schema.LoadResult{},
		Exports:      map[domain.SchemaVersion]*schema.ExportResult{},
		TruthByShard: map[string]*domain.ShardResult{},
	}

	for _, version := range versions {
		exported, err := schema.Export(project, version, schema.ExportOptions{IncludeShadow: true})
		if err != nil {
			return nil, fmt.Errorf("export %s: %w", version, err)
		}
		sample.Exports[version] = exported
		sample.Documents[version] = exported.Data
		loaded, err := schema.LoadDocument(exported.Data)
		if err != nil {
			return nil, fmt.Errorf("load exported %s: %w", version, err)
		}
		sample.Loaded[version] = loaded
	}

	for _, dlg := range project.Dialogues {
		for _, shard := range dlg.Shards {
			result, err := pipeline.BuildShard(shard, project.Params, dlg.ContentHash, domain.SchemaV3)
			if err != nil {
				return nil, fmt.Errorf("truth for %s: %w", shard.ID, err)
			}
			sample.TruthByShard[shard.ID] = result
			sample.TruthOrder = append(sample.TruthOrder, shard.ID)
		}
	}
	return sample, nil
}

func (s *Sample) TruthResults() []*domain.ShardResult {
	out := make([]*domain.ShardResult, 0, len(s.TruthOrder))
	for _, id := range s.TruthOrder {
		out = append(out, s.TruthByShard[id])
	}
	return out
}

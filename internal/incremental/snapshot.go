package incremental

import (
	"grayrelease/internal/engine"
	"grayrelease/internal/model"
)

type SnapshotCache struct {
	EngineeringVersion string   `json:"engineering_version"`
	Manager            *Manager `json:"-"`
	Replacements       int      `json:"replacements"`
	DiscardedCaches    int      `json:"discarded_caches"`
}

type ReplacementReport struct {
	PreviousVersion string `json:"previous_version"`
	CurrentVersion  string `json:"current_version"`
	VersionChanged  bool   `json:"version_changed"`
	CacheDiscarded  bool   `json:"cache_discarded"`
	Reason          string `json:"reason"`
}

func NewSnapshotCache(project *model.Project, requests []model.Request, config engine.Config) (*SnapshotCache, error) {
	manager, err := NewManager(project, requests, config)
	if err != nil {
		return nil, err
	}
	return &SnapshotCache{EngineeringVersion: project.EngineeringVersion, Manager: manager}, nil
}

func (c *SnapshotCache) Replace(project *model.Project, requests []model.Request, config engine.Config) (*ReplacementReport, error) {
	report := &ReplacementReport{
		PreviousVersion: c.EngineeringVersion,
		CurrentVersion:  project.EngineeringVersion,
		VersionChanged:  c.EngineeringVersion != project.EngineeringVersion,
	}
	if report.VersionChanged {
		c.Manager = nil
		c.DiscardedCaches++
		report.CacheDiscarded = true
		report.Reason = "engineering snapshot version changed; all route/mirror cache entries were discarded"
	} else {
		report.Reason = "same engineering version; existing cache may be retained for edit-scoped invalidation"
	}
	manager, err := NewManager(project, requests, config)
	if err != nil {
		return nil, err
	}
	c.Manager = manager
	c.EngineeringVersion = project.EngineeringVersion
	c.Replacements++
	return report, nil
}

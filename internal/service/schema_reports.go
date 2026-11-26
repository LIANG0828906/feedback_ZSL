package service

import "lip-pipeline-prototype/internal/domain"

type SchemaReport struct {
	Version           domain.SchemaVersion      `json:"version"`
	LoadReport        domain.LoadReport         `json:"load_report"`
	ExportLosses      []*domain.Error           `json:"export_losses"`
	ExportConversions []domain.ConversionRecord `json:"export_conversions"`
}

func (s *Service) SchemaReports() []SchemaReport {
	s.mu.RLock()
	defer s.mu.RUnlock()
	versions := []domain.SchemaVersion{domain.SchemaV1, domain.SchemaV2, domain.SchemaV3}
	out := make([]SchemaReport, 0, len(versions))
	for _, version := range versions {
		item := SchemaReport{Version: version}
		if loaded := s.sample.Loaded[version]; loaded != nil {
			item.LoadReport = loaded.Report
		}
		if exported := s.sample.Exports[version]; exported != nil {
			item.ExportLosses = exported.Losses
			item.ExportConversions = exported.Conversions
		}
		out = append(out, item)
	}
	return out
}

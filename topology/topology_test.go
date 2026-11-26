package topology_test

import (
	"strings"
	"testing"

	"maskqc/model"
	"maskqc/sample"
	"maskqc/topology"
)

func TestAnalyzeAndCompareTruth(t *testing.T) {
	bundle, err := sample.Generate(3001, []int{96})
	if err != nil {
		t.Fatal(err)
	}
	project := bundle.Projects[0].Canonical
	reference := make([]byte, project.Width*project.Height)
	for _, truth := range project.Truth {
		for index, value := range truth.ReferenceMask {
			if value >= model.ForegroundThreshold {
				reference[index] = 255
			}
		}
	}
	analysis, err := topology.Analyze(reference, project.Width, project.Height, project.Strokes)
	if err != nil {
		t.Fatal(err)
	}
	if len(analysis.Domains) != 3 {
		t.Fatalf("expected 3 domains, got %d", len(analysis.Domains))
	}
	for _, domain := range analysis.Domains {
		if domain.Area == 0 || domain.BBox.Empty() || domain.RingWidth == 0 || len(domain.Segments) == 0 {
			t.Fatalf("domain metrics are incomplete: %+v", domain)
		}
	}
	tolerance := topology.DefaultTolerance()
	tolerance.PixelPercent = 1
	issues := topology.Compare(analysis, project.Truth, tolerance)
	if len(issues) != 0 {
		t.Fatalf("reference analysis should match truth: %v", issues)
	}
}

func TestTopologyAndDirectionErrors(t *testing.T) {
	bundle, err := sample.Generate(3002, []int{96})
	if err != nil {
		t.Fatal(err)
	}
	project := bundle.Projects[0].Canonical
	reference := make([]byte, project.Width*project.Height)
	for _, truth := range project.Truth {
		for index, value := range truth.ReferenceMask {
			if value >= model.ForegroundThreshold {
				reference[index] = 255
			}
		}
	}
	analysis, err := topology.Analyze(reference, project.Width, project.Height, project.Strokes)
	if err != nil {
		t.Fatal(err)
	}
	tampered := append([]model.DomainTruth(nil), project.Truth...)
	tampered[0].Holes += 5
	issues := topology.Compare(analysis, tampered, topology.DefaultTolerance())
	if !model.HasCode(issues, model.CodeTopology) {
		t.Fatalf("hole tamper did not trigger E_TOP: %v", issues)
	}
	foundHole := false
	for _, issue := range issues {
		if strings.Contains(issue.Path, ".holes") && issue.Domain == 1 && issue.Measured != "" {
			foundHole = true
		}
	}
	if !foundHole {
		t.Fatalf("E_TOP lacks domain or measured value: %v", issues)
	}
	tampered = append([]model.DomainTruth(nil), project.Truth...)
	tampered[0].Direction = model.DirectionCoDirectional
	if tampered[0].Direction == analysis.Domains[0].Direction {
		tampered[0].Direction = model.DirectionCounter
	}
	issues = topology.Compare(analysis, tampered, topology.DefaultTolerance())
	if !model.HasCode(issues, model.CodeDirection) {
		t.Fatalf("direction tamper did not trigger E_DIR: %v", issues)
	}
	foundDirection := false
	for _, issue := range issues {
		if issue.Code == model.CodeDirection && issue.Domain == 1 && issue.Measured != "" {
			foundDirection = true
		}
	}
	if !foundDirection {
		t.Fatalf("E_DIR lacks domain or measured value: %v", issues)
	}
}

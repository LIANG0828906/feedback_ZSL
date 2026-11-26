package maskqa

import (
	"fmt"
	"image"
)

// VerifyMask extracts topology/directions and compares against truth.
func VerifyMask(project *Project, accumulated *image.Gray) (*VerificationReport, error) {
	if project == nil {
		return nil, decodeError("project", "nil project")
	}
	topo := ExtractTopology(accumulated, DefaultThreshold)
	mapping := matchTruth(topo.Components, project.Truth)
	directions := VerifyDirections(accumulated, topo, project, mapping)
	dirByDomain := map[int]*DirectionReport{}
	for i := range directions {
		dirByDomain[directions[i].DomainNo] = &directions[i]
	}

	report := &VerificationReport{TopologyOK: true, DirectionOK: true}
	var topErr *Error
	if len(topo.Components) != len(project.Truth) {
		report.TopologyOK = false
		topErr = &Error{
			Code: CodeTop, Field: "domains.length",
			Message:  fmt.Sprintf("component count measured=%d truth=%d", len(topo.Components), len(project.Truth)),
			Expected: len(project.Truth), Actual: len(topo.Components),
		}
		report.Issues = append(report.Issues, Issue{
			Code: CodeTop, Field: "domains.length",
			Message: topErr.Message, Expected: topErr.Expected, Actual: topErr.Actual,
		})
	}

	for _, comp := range topo.Components {
		truthNo := mapping[comp.DomainNo]
		var truth DomainTruth
		for _, candidate := range project.Truth {
			if candidate.DomainNo == truthNo {
				truth = candidate
				break
			}
		}
		deviations := compareDomain(comp, truth, project.Tolerances)
		domainReport := DomainReport{
			DomainNo: comp.DomainNo, TruthNo: truthNo,
			Matched: truthNo != 0, Component: comp, Truth: truth,
			Deviations: deviations, Direction: dirByDomain[comp.DomainNo],
		}
		if domainReport.Direction != nil && !domainReport.Direction.Consistent {
			report.DirectionOK = false
		}
		for _, dev := range deviations {
			report.TopologyOK = false
			issue := Issue{
				Code: CodeTop, Field: fmt.Sprintf("domains[%d].%s", comp.DomainNo-1, dev.Field),
				DomainNo: comp.DomainNo, Message: dev.Message,
				Expected: dev.Expected, Actual: dev.Actual,
			}
			report.Issues = append(report.Issues, issue)
			if topErr == nil && (dev.Field == "holes" || dev.Field == "band_width_milli") {
				topErr = &Error{
					Code: CodeTop, Field: issue.Field, DomainNo: comp.DomainNo,
					Message: dev.Message, Expected: dev.Expected, Actual: dev.Actual,
				}
			}
		}
		report.Domains = append(report.Domains, domainReport)
	}
	if topErr == nil && !report.TopologyOK && len(report.Issues) > 0 {
		first := report.Issues[0]
		topErr = &Error{
			Code: CodeTop, Field: first.Field, DomainNo: first.DomainNo,
			Message: first.Message, Expected: first.Expected, Actual: first.Actual,
		}
	}
	if topErr != nil {
		return report, topErr
	}
	return report, nil
}

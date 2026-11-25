package acoustic

import (
	"fmt"
	"math"
)

type TruthMismatch struct {
	ListenerID  int     `json:"listenerId"`
	SourceID    int     `json:"sourceId,omitempty"`
	ReflectorID int     `json:"reflectorId,omitempty"`
	Field       string  `json:"field"`
	Expected    any     `json:"expected"`
	Actual      any     `json:"actual"`
	Tolerance   float64 `json:"tolerance,omitempty"`
}

type TruthComparison struct {
	Passed        bool            `json:"passed"`
	ExpectedPaths int             `json:"expectedPaths"`
	ActualPaths   int             `json:"actualPaths"`
	Mismatches    []TruthMismatch `json:"mismatches,omitempty"`
}

func CompareTruth(project *Project, solutions []ListenerSolution) TruthComparison {
	comparison := TruthComparison{Passed: true}
	truthByListener := make(map[int]ListenerTruth, len(project.Truth))
	for _, truth := range project.Truth {
		truthByListener[truth.ListenerID] = truth
		comparison.ExpectedPaths += truth.ValidPathCount
	}
	solutionByListener := make(map[int]ListenerSolution, len(solutions))
	for _, solution := range solutions {
		solutionByListener[solution.ListenerID] = solution
		comparison.ActualPaths += solution.ValidPathCount
	}
	for _, listener := range project.Listeners {
		truth, truthOK := truthByListener[listener.ID]
		solution, solutionOK := solutionByListener[listener.ID]
		if !truthOK {
			comparison.addMismatch(TruthMismatch{ListenerID: listener.ID, Field: "truth", Expected: "present", Actual: "missing"})
			continue
		}
		if !solutionOK {
			comparison.addMismatch(TruthMismatch{ListenerID: listener.ID, Field: "solution", Expected: "present", Actual: "missing"})
			continue
		}
		if solution.RoomID != truth.RoomID {
			comparison.addMismatch(TruthMismatch{ListenerID: listener.ID, Field: "roomId", Expected: truth.RoomID, Actual: solution.RoomID})
		}
		if solution.ValidPathCount != truth.ValidPathCount {
			comparison.addMismatch(TruthMismatch{ListenerID: listener.ID, Field: "validPathCount", Expected: truth.ValidPathCount, Actual: solution.ValidPathCount})
		}
		truthSources := make(map[int]TruthSource, len(truth.Sources))
		for _, sourceTruth := range truth.Sources {
			truthSources[sourceTruth.SourceID] = sourceTruth
		}
		actualSources := make(map[int]SourceReflectionResult, len(solution.Sources))
		for _, source := range solution.Sources {
			actualSources[source.SourceID] = source
		}
		for _, source := range project.Sources {
			expected, expectedOK := truthSources[source.ID]
			actual, actualOK := actualSources[source.ID]
			if !expectedOK {
				comparison.addMismatch(TruthMismatch{ListenerID: listener.ID, SourceID: source.ID, Field: "truthSource", Expected: "present", Actual: "missing"})
				continue
			}
			if !actualOK {
				comparison.addMismatch(TruthMismatch{ListenerID: listener.ID, SourceID: source.ID, Field: "sourceResult", Expected: "present", Actual: "missing"})
				continue
			}
			if expected.ValidPathCount != len(actual.Paths) {
				comparison.addMismatch(TruthMismatch{
					ListenerID: listener.ID,
					SourceID:   source.ID,
					Field:      "sourceValidPathCount",
					Expected:   expected.ValidPathCount,
					Actual:     len(actual.Paths),
				})
			}
			compareChain(&comparison, project.Tolerance.PathTicks, listener.ID, source.ID, expected, actual.Chain)
			expectedPaths := make(map[int]TruthPath, len(expected.Paths))
			for _, path := range expected.Paths {
				expectedPaths[path.ReflectorID] = path
			}
			actualPaths := make(map[int]ReflectionPath, len(actual.Paths))
			for _, path := range actual.Paths {
				actualPaths[path.ReflectorID] = path
			}
			for _, truthPath := range expected.Paths {
				actualPath, ok := actualPaths[truthPath.ReflectorID]
				if !ok {
					comparison.addMismatch(TruthMismatch{
						ListenerID:  listener.ID,
						SourceID:    source.ID,
						ReflectorID: truthPath.ReflectorID,
						Field:       "reflectionPath",
						Expected:    "present",
						Actual:      "missing",
					})
					continue
				}
				comparePath(&comparison, project.Tolerance, listener.ID, source.ID, truthPath, actualPath)
			}
			for _, actualPath := range actual.Paths {
				if _, ok := expectedPaths[actualPath.ReflectorID]; !ok {
					comparison.addMismatch(TruthMismatch{
						ListenerID:  listener.ID,
						SourceID:    source.ID,
						ReflectorID: actualPath.ReflectorID,
						Field:       "reflectionPath",
						Expected:    "absent",
						Actual:      "present",
					})
				}
			}
		}
	}
	comparison.Passed = len(comparison.Mismatches) == 0
	return comparison
}

func compareChain(comparison *TruthComparison, pathTolerance Fixed, listenerID, sourceID int, expected TruthSource, actual ChainSummary) {
	if len(expected.ChainRoomIDs) != len(actual.RoomIDs) {
		comparison.addMismatch(TruthMismatch{
			ListenerID: listenerID,
			SourceID:   sourceID,
			Field:      "chainRoomIds",
			Expected:   expected.ChainRoomIDs,
			Actual:     actual.RoomIDs,
		})
	} else {
		for i := range expected.ChainRoomIDs {
			if expected.ChainRoomIDs[i] != actual.RoomIDs[i] {
				comparison.addMismatch(TruthMismatch{
					ListenerID: listenerID,
					SourceID:   sourceID,
					Field:      "chainRoomIds",
					Expected:   expected.ChainRoomIDs,
					Actual:     actual.RoomIDs,
				})
				break
			}
		}
	}
	if len(expected.ChainOpeningIDs) != len(actual.OpeningIDs) {
		comparison.addMismatch(TruthMismatch{
			ListenerID: listenerID,
			SourceID:   sourceID,
			Field:      "chainOpeningIds",
			Expected:   expected.ChainOpeningIDs,
			Actual:     actual.OpeningIDs,
		})
	} else {
		for i := range expected.ChainOpeningIDs {
			if expected.ChainOpeningIDs[i] != actual.OpeningIDs[i] {
				comparison.addMismatch(TruthMismatch{
					ListenerID: listenerID,
					SourceID:   sourceID,
					Field:      "chainOpeningIds",
					Expected:   expected.ChainOpeningIDs,
					Actual:     actual.OpeningIDs,
				})
				break
			}
		}
	}
	delta := math.Abs(float64(expected.ChainLengthTicks - actual.LengthTicks))
	if delta > float64(pathTolerance) {
		comparison.addMismatch(TruthMismatch{
			ListenerID: listenerID,
			SourceID:   sourceID,
			Field:      "chainLengthTicks",
			Expected:   expected.ChainLengthTicks,
			Actual:     actual.LengthTicks,
			Tolerance:  float64(pathTolerance),
		})
	}
}

func comparePath(comparison *TruthComparison, tolerance Tolerance, listenerID, sourceID int, expected TruthPath, actual ReflectionPath) {
	if absInt64(expected.ReflectionPoint.X-actual.ReflectionPoint.X) > tolerance.CoordinateTicks {
		comparison.addMismatch(TruthMismatch{ListenerID: listenerID, SourceID: sourceID, ReflectorID: expected.ReflectorID, Field: "reflectionPoint.x", Expected: expected.ReflectionPoint.X, Actual: actual.ReflectionPoint.X, Tolerance: float64(tolerance.CoordinateTicks)})
	}
	if absInt64(expected.ReflectionPoint.Y-actual.ReflectionPoint.Y) > tolerance.CoordinateTicks {
		comparison.addMismatch(TruthMismatch{ListenerID: listenerID, SourceID: sourceID, ReflectorID: expected.ReflectorID, Field: "reflectionPoint.y", Expected: expected.ReflectionPoint.Y, Actual: actual.ReflectionPoint.Y, Tolerance: float64(tolerance.CoordinateTicks)})
	}
	if absInt64(expected.PathLengthTicks-actual.PathLengthTicks) > tolerance.PathTicks {
		comparison.addMismatch(TruthMismatch{ListenerID: listenerID, SourceID: sourceID, ReflectorID: expected.ReflectorID, Field: "pathLengthTicks", Expected: expected.PathLengthTicks, Actual: actual.PathLengthTicks, Tolerance: float64(tolerance.PathTicks)})
	}
	if absInt64(expected.IncidenceCos-actual.IncidenceCos) > tolerance.IncidenceCos {
		comparison.addMismatch(TruthMismatch{ListenerID: listenerID, SourceID: sourceID, ReflectorID: expected.ReflectorID, Field: "incidenceCos", Expected: expected.IncidenceCos, Actual: actual.IncidenceCos, Tolerance: float64(tolerance.IncidenceCos)})
	}
	if math.Abs(expected.SPLDB-actual.SPLDB) > tolerance.SPLDB {
		comparison.addMismatch(TruthMismatch{ListenerID: listenerID, SourceID: sourceID, ReflectorID: expected.ReflectorID, Field: "splDb", Expected: expected.SPLDB, Actual: actual.SPLDB, Tolerance: tolerance.SPLDB})
	}
}

func (comparison *TruthComparison) addMismatch(mismatch TruthMismatch) {
	comparison.Mismatches = append(comparison.Mismatches, mismatch)
}

func (comparison TruthComparison) Error() error {
	if comparison.Passed {
		return nil
	}
	first := comparison.Mismatches[0]
	return &Error{
		Code:         CodeTruth,
		Op:           "truth_compare",
		RoomIDs:      nil,
		SourceIDs:    optionalID(first.SourceID),
		ListenerIDs:  optionalID(first.ListenerID),
		ReflectorIDs: optionalID(first.ReflectorID),
		Message:      fmt.Sprintf("truth mismatch at %s: expected=%v actual=%v", first.Field, first.Expected, first.Actual),
	}
}

func optionalID(id int) []int {
	if id == 0 {
		return nil
	}
	return []int{id}
}

func CountTolerancePasses(solutions []ListenerSolution, splToleranceDB float64, coordinateToleranceTicks float64) int {
	count := 0
	for _, solution := range solutions {
		for _, path := range solution.Paths {
			report := path.ErrorPropagation
			if report.CumulativeUpperBoundDB <= splToleranceDB && report.MirrorResidualTicks <= coordinateToleranceTicks {
				count++
			}
		}
	}
	return count
}

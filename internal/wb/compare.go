package wb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

func (e *Engine) CompareWithFull(ctx context.Context) (*DiffReport, *APIError) {
	e.mu.Lock()
	defer e.mu.Unlock()

	full, apiErr := NewEngine(CloneSample(e.sample), nil)
	if apiErr != nil {
		return nil, apiErr
	}
	if _, runErr := full.RunFull(ctx, false); runErr != nil && !isConnOnlyError(runErr) {
		return nil, runErr
	}

	report := CompareResultMaps(e.sample, e.results, full.results,
		e.commitSequenceLocked(), full.commitSequenceLocked())
	e.lastDiff = report
	if !report.Match {
		return report, NewAPIError(CodeDiff, report.FirstLocator, "diff",
			"incremental and full recompute differ at %s", report.FirstLocator)
	}
	return report, nil
}

func isConnOnlyError(err *APIError) bool {
	return err != nil && err.Code == CodeConn
}

func CompareResultMaps(s *Sample, actual map[string]ListenerResult, expected map[string]ListenerResult, actualSeq, expectedSeq []string) *DiffReport {
	if mismatch := compareValue("commitSequence", expectedSeq, actualSeq); mismatch != nil {
		mismatch.IncrementalHash = hashComparison(actual, actualSeq)
		mismatch.FullHash = hashComparison(expected, expectedSeq)
		return mismatch
	}
	for _, listener := range s.Listeners {
		locator := "listeners." + listener.ID
		actualResult, actualOK := actual[listener.ID]
		expectedResult, expectedOK := expected[listener.ID]
		if actualOK != expectedOK {
			return mismatchReport(locator+".exists", expectedOK, actualOK)
		}
		if !actualOK {
			continue
		}
		if actualResult.RoomID != expectedResult.RoomID {
			return mismatchReport(locator+".roomId", expectedResult.RoomID, actualResult.RoomID)
		}
		if len(actualResult.Pairs) != len(expectedResult.Pairs) {
			return mismatchReport(locator+".pairs.length", len(expectedResult.Pairs), len(actualResult.Pairs))
		}
		for i := range expectedResult.Pairs {
			expectedPair := expectedResult.Pairs[i]
			actualPair := actualResult.Pairs[i]
			prefix := fmt.Sprintf("%s.pairs[%s->%s]", locator, expectedPair.Path.SourceID, expectedPair.Path.ListenerID)
			if expectedPair.Error != nil || actualPair.Error != nil {
				if expectedPair.Error == nil || actualPair.Error == nil ||
					expectedPair.Error.Code != actualPair.Error.Code ||
					expectedPair.Error.Object != actualPair.Error.Object {
					return mismatchReport(prefix+".error", errorBrief(expectedPair.Error), errorBrief(actualPair.Error))
				}
				continue
			}
			if mismatch := comparePath(prefix+".path", expectedPair.Path, actualPair.Path); mismatch != nil {
				return mismatch
			}
			if expectedPair.ExpectedSPLDB != actualPair.ExpectedSPLDB {
				return mismatchReport(prefix+".expectedSplDb", expectedPair.ExpectedSPLDB, actualPair.ExpectedSPLDB)
			}
			if len(expectedPair.Samples) != len(actualPair.Samples) {
				return mismatchReport(prefix+".samples.length", len(expectedPair.Samples), len(actualPair.Samples))
			}
			for sampleIndex := range expectedPair.Samples {
				expectedSample := expectedPair.Samples[sampleIndex]
				actualSample := actualPair.Samples[sampleIndex]
				sampleLocator := fmt.Sprintf("%s.samples[%d]", prefix, sampleIndex)
				if expectedSample.TMS != actualSample.TMS {
					return mismatchReport(sampleLocator+".tMs", expectedSample.TMS, actualSample.TMS)
				}
				if expectedSample.LevelDB != actualSample.LevelDB {
					return mismatchReport(sampleLocator+".levelDb", expectedSample.LevelDB, actualSample.LevelDB)
				}
			}
		}
	}
	return &DiffReport{
		Match:           true,
		IncrementalHash: hashComparison(actual, actualSeq),
		FullHash:        hashComparison(expected, expectedSeq),
	}
}

func comparePath(prefix string, expected, actual PathResult) *DiffReport {
	checks := []struct {
		name     string
		expected any
		actual   any
	}{
		{"sourceRoom", expected.SourceRoom, actual.SourceRoom},
		{"listenerRoom", expected.ListenerRoom, actual.ListenerRoom},
		{"roomChain", expected.RoomChain, actual.RoomChain},
		{"doorChain", expected.DoorChain, actual.DoorChain},
		{"length", expected.Length, actual.Length},
		{"occlusionCount", expected.OcclusionCount, actual.OcclusionCount},
		{"occlusionDb", expected.OcclusionDB, actual.OcclusionDB},
	}
	for _, check := range checks {
		if mismatch := compareValue(prefix+"."+check.name, check.expected, check.actual); mismatch != nil {
			return mismatch
		}
	}
	return nil
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func compareValue(locator string, expected, actual any) *DiffReport {
	eb, _ := json.Marshal(expected)
	ab, _ := json.Marshal(actual)
	if string(eb) == string(ab) {
		return nil
	}
	return mismatchReport(locator, expected, actual)
}

func mismatchReport(locator string, expected, actual any) *DiffReport {
	return &DiffReport{
		Match:        false,
		FirstLocator: locator,
		Expected:     expected,
		Actual:       actual,
	}
}

func errorBrief(err *APIError) any {
	if err == nil {
		return nil
	}
	return map[string]string{"code": err.Code, "object": err.Object}
}

func hashComparison(results map[string]ListenerResult, sequence []string) string {
	b, _ := json.Marshal(struct {
		Results  map[string]ListenerResult `json:"results"`
		Sequence []string                  `json:"sequence"`
	}{results, sequence})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (e *Engine) VerifyInitialTruth() *APIError {
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, tp := range e.sample.Truth.Paths {
		result, ok := e.results[tp.ListenerID]
		if !ok {
			return NewAPIError(CodeDiff, tp.ListenerID, "listenerId", "listener result missing")
		}
		var pair *PairResult
		for i := range result.Pairs {
			if result.Pairs[i].Path.SourceID == tp.SourceID {
				pair = &result.Pairs[i]
				break
			}
		}
		if pair == nil {
			return NewAPIError(CodeDiff, tp.SourceID+"/"+tp.ListenerID, "sourceId", "pair result missing")
		}
		if pair.Error != nil {
			return pair.Error
		}
		if !stringSlicesEqual(pair.Path.RoomChain, tp.RoomChain) {
			return NewAPIError(CodeDiff, tp.SourceID+"/"+tp.ListenerID, "roomChain",
				"truth chain %v != computed %v at first item", tp.RoomChain, pair.Path.RoomChain)
		}
		if pair.Path.Length != tp.Length {
			return NewAPIError(CodeDiff, tp.SourceID+"/"+tp.ListenerID, "length",
				"truth length %d != computed %d", tp.Length, pair.Path.Length)
		}
		if absFixed(pair.ExpectedSPLDB-tp.ExpectedSPLDB) > e.sample.Precision.ToleranceDB {
			return NewAPIError(CodeDiff, tp.SourceID+"/"+tp.ListenerID, "expectedSplDb",
				"SPL %.3f differs from truth %.3f beyond tolerance", pair.ExpectedSPLDB.Float64(), tp.ExpectedSPLDB.Float64())
		}
		if len(pair.Samples) != len(tp.DecaySamplesDB) {
			return NewAPIError(CodeDiff, tp.SourceID+"/"+tp.ListenerID, "samples", "sample count mismatch")
		}
		for i, sample := range pair.Samples {
			if absFixed(sample.LevelDB-tp.DecaySamplesDB[i]) > e.sample.Precision.ToleranceDB {
				return NewAPIError(CodeDiff, fmt.Sprintf("%s/%s/samples[%d]", tp.SourceID, tp.ListenerID, i),
					"levelDb", "decay sample differs beyond tolerance")
			}
		}
	}
	truthRooms := e.sample.Truth.ListenerRooms
	for listenerID, expectedRoom := range truthRooms {
		if result, ok := e.results[listenerID]; !ok || result.RoomID != expectedRoom {
			actual := ""
			if ok {
				actual = result.RoomID
			}
			return NewAPIError(CodeDiff, listenerID, "listenerRoom",
				"truth room %s != computed %s", expectedRoom, actual)
		}
	}
	return nil
}

package dressbatch

import (
	"math/rand"
	"sort"
	"sync"
	"testing"
)

func TestGeneratedTruthAndDeterministicPacking(t *testing.T) {
	projects, err := GenerateSampleSuite(42, 4)
	if err != nil {
		t.Fatal(err)
	}
	for _, project := range projects {
		report, err := VerifyTruth(project)
		if err != nil {
			t.Fatalf("%s truth: %v", project.ID, err)
		}
		if !report.Passed || len(report.Cases) != 6 {
			t.Fatalf("%s unexpected truth report: %+v", project.ID, report)
		}
		legal := 0
		for _, sample := range project.Truth.Assemblies {
			if !sample.ExpectedLegal {
				continue
			}
			legal++
			result, err := Pack(project, sample.CandidatePartIDs)
			if err != nil {
				t.Fatalf("%s/%s pack: %v", project.ID, sample.ID, err)
			}
			if len(result.Batches) == 0 {
				t.Fatalf("%s/%s produced no batches", project.ID, sample.ID)
			}
			sequence := 0
			for _, batch := range result.Batches {
				if len(batch.MaterialSlots) > project.Constraints.MaxMaterialSlotsPerBatch {
					t.Fatalf("%s batch %s exceeds material slots", project.ID, batch.ID)
				}
				if batch.VertexUsed > project.Constraints.MaxVerticesPerBatch {
					t.Fatalf("%s batch %s exceeds vertices", project.ID, batch.ID)
				}
				if batch.IndexUsed > project.Constraints.MaxIndicesPerBatch {
					t.Fatalf("%s batch %s exceeds indices", project.ID, batch.ID)
				}
				for _, decision := range batch.Decisions {
					sequence++
					if decision.Sequence != sequence {
						t.Fatalf("%s decision sequence expected %d got %d", project.ID, sequence, decision.Sequence)
					}
				}
				assertPlacementsContiguous(t, project, batch)
			}
		}
		if legal == 0 {
			t.Fatalf("%s has no legal samples", project.ID)
		}
	}
}

func assertPlacementsContiguous(t *testing.T, project *Project, batch Batch) {
	t.Helper()
	byID := partMap(project)
	vertexCursor := 0
	indexCursor := 0
	for _, placement := range batch.Placements {
		part := byID[placement.PartID]
		if placement.VertexOffset != vertexCursor || placement.VertexEnd != vertexCursor+part.VertexCount {
			t.Fatalf("batch %s part %s vertex offset mismatch", batch.ID, part.ID)
		}
		if placement.IndexOffset != indexCursor || placement.IndexEnd != indexCursor+part.EffectiveIndexCount() {
			t.Fatalf("batch %s part %s index offset mismatch", batch.ID, part.ID)
		}
		for _, submesh := range placement.Submeshes {
			if submesh.VertexStart != placement.VertexStart+submesh.SourceVertexStart {
				t.Fatalf("batch %s part %s submesh %s vertex offset mismatch", batch.ID, part.ID, submesh.SubmeshID)
			}
			if submesh.IndexStart != placement.IndexStart+submesh.SourceIndexStart {
				t.Fatalf("batch %s part %s submesh %s index offset mismatch", batch.ID, part.ID, submesh.SubmeshID)
			}
		}
		vertexCursor += part.VertexCount
		indexCursor += part.EffectiveIndexCount()
	}
}

func TestAssemblyOrderInvariantAndConflictCategories(t *testing.T) {
	project := testProject(t)
	for _, sample := range project.Truth.Assemblies {
		expected, err := EvaluateAssembly(project, sample.CandidatePartIDs)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 50; i++ {
			candidates := append([]string(nil), sample.CandidatePartIDs...)
			rand.New(rand.NewSource(int64(i))).Shuffle(len(candidates), func(a, b int) {
				candidates[a], candidates[b] = candidates[b], candidates[a]
			})
			actual, err := EvaluateAssembly(project, candidates)
			if err != nil {
				t.Fatal(err)
			}
			if !equalStrings(actual.CandidatePartIDs, expected.CandidatePartIDs) ||
				!equalStrings(actual.AcceptedPartIDs, expected.AcceptedPartIDs) ||
				!equalStrings(actual.RejectedPartIDs, expected.RejectedPartIDs) ||
				actual.Legal != expected.Legal ||
				!conflictsEqual(actual.Conflicts, expected.Conflicts) {
				t.Fatalf("assembly changed with input order:\n%+v\n%+v", expected, actual)
			}
		}
	}
	categories := map[string]bool{}
	for _, sample := range project.Truth.Assemblies {
		for _, conflict := range sample.ExpectedConflicts {
			categories[conflict.Category] = true
		}
	}
	for _, category := range []string{ConflictMount, ConflictRule, ConflictSkin} {
		if !categories[category] {
			t.Fatalf("generated samples do not cover conflict category %s", category)
		}
	}
}

func TestAssemblyConcurrentDeterminism(t *testing.T) {
	project := testProject(t)
	sample := project.Truth.Assemblies[5]
	expected, err := EvaluateAssembly(project, sample.CandidatePartIDs)
	if err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	results := make(chan *AssemblyResult, 64)
	for i := 0; i < 64; i++ {
		wait.Add(1)
		go func(seed int64) {
			defer wait.Done()
			candidates := append([]string(nil), sample.CandidatePartIDs...)
			rand.New(rand.NewSource(seed)).Shuffle(len(candidates), func(a, b int) {
				candidates[a], candidates[b] = candidates[b], candidates[a]
			})
			actual, err := EvaluateAssembly(project, candidates)
			if err != nil {
				t.Errorf("EvaluateAssembly: %v", err)
				return
			}
			results <- actual
		}(int64(i))
	}
	wait.Wait()
	close(results)
	for actual := range results {
		if !conflictsEqual(actual.Conflicts, expected.Conflicts) ||
			!equalStrings(actual.AcceptedPartIDs, expected.AcceptedPartIDs) {
			t.Fatalf("concurrent evaluation changed result: %+v", actual)
		}
	}
}

func TestPackPermutationInvariant(t *testing.T) {
	project := testProject(t)
	candidate := testLegalCandidate(t, project)
	expected, err := Pack(project, candidate)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 25; i++ {
		permuted := append([]string(nil), candidate...)
		rand.New(rand.NewSource(int64(i+10))).Shuffle(len(permuted), func(a, b int) {
			permuted[a], permuted[b] = permuted[b], permuted[a]
		})
		actual, err := Pack(project, permuted)
		if err != nil {
			t.Fatal(err)
		}
		if err := ComparePackResults(expected, actual); err != nil {
			t.Fatalf("permutation %d changed pack: %v", i, err)
		}
	}
}

func TestPackReturnsEPackWithBudgets(t *testing.T) {
	project := testProject(t)
	candidate := testLegalCandidate(t, project)
	project.Constraints.MaxVerticesPerBatch = 1
	_, err := Pack(project, candidate)
	if err == nil {
		t.Fatal("expected E_PACK")
	}
	domainErr, ok := AsDomainError(err)
	if !ok || domainErr.Code != CodePack {
		t.Fatalf("expected E_PACK, got %v", err)
	}
	for _, key := range []string{"remaining_vertex_budget", "part_vertex_count"} {
		if _, exists := domainErr.Details[key]; !exists {
			t.Fatalf("E_PACK missing detail %s", key)
		}
	}
}

func TestPackIllegalAssemblyReturnsEAssembly(t *testing.T) {
	project := testProject(t)
	var illegal *TruthAssembly
	for i := range project.Truth.Assemblies {
		if !project.Truth.Assemblies[i].ExpectedLegal {
			illegal = &project.Truth.Assemblies[i]
			break
		}
	}
	if illegal == nil {
		t.Fatal("no illegal sample")
	}
	_, err := Pack(project, illegal.CandidatePartIDs)
	if err == nil || ErrorCode(err) != CodeAssembly {
		t.Fatalf("expected E_ASSEMBLY, got %v", err)
	}
}

func TestDecisionTraceStable(t *testing.T) {
	project := testProject(t)
	result, _ := testPack(t, project)
	var first []Decision
	for _, batch := range result.Batches {
		first = append(first, batch.Decisions...)
	}
	again, err := Pack(project, result.CandidatePartIDs)
	if err != nil {
		t.Fatal(err)
	}
	var second []Decision
	for _, batch := range again.Batches {
		second = append(second, batch.Decisions...)
	}
	if len(first) != len(second) {
		t.Fatalf("decision count differs")
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("decision %d differs: %+v vs %+v", i, first[i], second[i])
		}
	}
	partIDs := make([]string, 0, len(first))
	for _, decision := range first {
		partIDs = append(partIDs, decision.PartID)
	}
	sort.Strings(partIDs)
}

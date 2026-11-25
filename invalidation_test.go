package acoustic_test

import (
	"strings"
	"testing"

	acoustic "levelacoustic"
)

func TestReflectorChangeInvalidatesOnlyItsRoom(t *testing.T) {
	oldProject := sampleProject(t)
	cached := solveAllListeners(t, oldProject)
	newProject := cloneProject(t, oldProject)
	newProject.Reflectors[0].Absorption = 0.11
	report, err := acoustic.PlanInvalidation(oldProject, newProject, cached)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.InvalidatedRooms) != 1 || report.InvalidatedRooms[0] != 1 {
		t.Fatalf("unexpected invalidated rooms: %v", report.InvalidatedRooms)
	}
	if len(report.ReusedRooms) != 2 || report.ReusedRooms[0] != 2 || report.ReusedRooms[1] != 3 {
		t.Fatalf("unexpected reused rooms: %v", report.ReusedRooms)
	}
	entry := report.Entries[0]
	if !entry.Invalidated || entry.Reused || len(entry.InvalidatedListeners) != 1 || entry.InvalidatedListeners[0] != 21 {
		t.Fatalf("unexpected room 1 entry: %+v", entry)
	}
	joined := strings.Join(entry.Reasons, " | ")
	if !strings.Contains(joined, "reflector 31") {
		t.Fatalf("missing reflector reason: %s", joined)
	}
	if !hasDependency(report.Dependencies, acoustic.DependencyReflector, acoustic.DependencyListenerSolve) {
		t.Fatal("reflector to listener solve dependency not recorded")
	}
}

func TestUnusedDoorAdditionReusesAllRooms(t *testing.T) {
	oldProject := sampleProject(t)
	cached := solveAllListeners(t, oldProject)
	newProject := cloneProject(t, oldProject)
	newProject.Openings = append(newProject.Openings, acoustic.Opening{ID: 203, WallID: 101, Start: 800, Width: 200})
	if err := newProject.Validate(); err != nil {
		t.Fatalf("edited project should be valid: %v", err)
	}
	report, err := acoustic.PlanInvalidation(oldProject, newProject, cached)
	if err != nil {
		t.Fatal(err)
	}
	if report.ConnectivityChanged {
		t.Fatal("unused door must not change room chains")
	}
	if len(report.InvalidatedRooms) != 0 {
		t.Fatalf("unused door invalidated rooms: %v", report.InvalidatedRooms)
	}
	if len(report.ReusedRooms) != 3 {
		t.Fatalf("unexpected reused rooms: %v", report.ReusedRooms)
	}
	for _, entry := range report.Entries {
		if !entry.Reused || entry.Invalidated || len(entry.InvalidatedListeners) != 0 {
			t.Fatalf("unexpected invalidation entry: %+v", entry)
		}
	}
	if !hasDependency(report.Dependencies, acoustic.DependencyDoor, acoustic.DependencyChain) {
		t.Fatal("door to chain dependency not recorded")
	}
	if !hasDependency(report.Dependencies, acoustic.DependencyChain, acoustic.DependencyListenerSolve) {
		t.Fatal("chain to listener solve dependency not recorded")
	}
}

func hasDependency(edges []acoustic.DependencyEdge, fromKind, toKind acoustic.DependencyKind) bool {
	for _, edge := range edges {
		if edge.From.Kind == fromKind && edge.To.Kind == toKind {
			return true
		}
	}
	return false
}

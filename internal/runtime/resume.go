package runtime

import (
	"sort"

	"presentkernel/internal/checkpoint"
	"presentkernel/internal/domain"
	"presentkernel/internal/engine"
)

type ResumeOutcome struct {
	Snapshot    *Snapshot `json:"snapshot"`
	Interrupted bool      `json:"interrupted"`
	Invalidated bool      `json:"invalidated"`
	StopAfter   int       `json:"stop_after_new_commits"`
}

// RunResume opens a SQLite checkpoint, validates the complete ordered
// snapshot, discards that run snapshot if any key/version mismatches, and
// continues from committed/content-addressed partitions. StopAfterNew > 0
// simulates a crash after that many newly committed partitions.
func RunResume(p *domain.Project, dbPath, runID string, stopAfterNew int, faults map[string]string) (*ResumeOutcome, error) {
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		return nil, err
	}
	defer store.Close()

	fps := p.TargetFPS
	expected := ExpectedCacheKeys(p, fps)
	valid, reason, _, err := store.ValidateRun(runID, expected)
	if err != nil {
		return nil, err
	}
	invalidated := false
	if !valid {
		invalidated = true
	}
	existing := map[string]*domain.PartitionResult{}
	if valid {
		loaded, _, err := store.LoadRun(runID)
		if err != nil {
			return nil, err
		}
		for _, r := range loaded {
			existing[r.PartitionID] = r
		}
	}

	refs := Refs(p)
	snap := &Snapshot{Path: "resume", FPS: fps, Errors: map[string]*domain.Error{}}
	newCommits := 0
	interrupted := false
	for position, ref := range refs {
		sequence := position + 1
		if r, ok := existing[ref.PartitionID]; ok {
			r.CommitSequence = sequence
			snap.Results = append(snap.Results, r)
			snap.Sequence = append(snap.Sequence, ref.PartitionID)
			snap.CacheHits = append(snap.CacheHits, ref.PartitionID)
			continue
		}

		di := engine.FindDialogueIndex(p, ref.DialogueID)
		_, _, key := engine.PartitionCacheKey(p, di, ref.PartitionIndex, fps)
		var r *domain.PartitionResult
		if cached, ok, err := store.GetCached(key); err != nil {
			return nil, err
		} else if ok {
			r = cached
			snap.CacheHits = append(snap.CacheHits, ref.PartitionID+":content-cache")
		} else {
			fault := ""
			if faults != nil {
				fault = faults[ref.PartitionID]
			}
			built, berr := engine.BuildPartition(p, di, ref.PartitionIndex, engine.Options{FPS: fps, Fault: fault})
			if berr != nil {
				if de, ok := domain.AsError(berr); ok {
					snap.Errors[ref.PartitionID] = de
				}
				continue
			}
			r = built
			snap.Rebuilt = append(snap.Rebuilt, ref.PartitionID)
		}
		r.CommitSequence = sequence
		if err := store.CommitPartition(runID, sequence, r); err != nil {
			return nil, err
		}
		newCommits++
		snap.Results = append(snap.Results, r)
		snap.Sequence = append(snap.Sequence, ref.PartitionID)
		if stopAfterNew > 0 && newCommits >= stopAfterNew {
			interrupted = true
			break
		}
	}

	if !interrupted {
		for _, ref := range refs {
			found := false
			for _, id := range snap.Sequence {
				if id == ref.PartitionID {
					found = true
					break
				}
			}
			if !found {
				break
			}
		}
	}
	st, err := store.Status(runID)
	if err == nil {
		snap.CheckpointStatus = st
		if !valid {
			st.InvalidReason = reason
		}
	}
	if len(snap.Errors) == 0 {
		snap.Errors = nil
	}
	sort.Slice(snap.Results, func(i, j int) bool { return snap.Results[i].PartitionID < snap.Results[j].PartitionID })
	return &ResumeOutcome{Snapshot: snap, Interrupted: interrupted, Invalidated: invalidated, StopAfter: stopAfterNew}, nil
}

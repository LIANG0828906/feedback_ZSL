package loader

import (
	"fmt"

	"audiobus/domain"
	"audiobus/errs"
)

func ValidateFlags(events []domain.Event, m *domain.Manifest, injectedConflictIDs ...int64) error {
	conflict := map[int64]bool{}
	for _, id := range injectedConflictIDs {
		conflict[id] = true
	}
	for _, e := range events {
		if !e.Conditional {
			continue
		}
		if conflict[e.ID] {
			return flagError(e, "injected contradictory conditional flag resolution")
		}
		if e.Flag < 0 || (m.FlagMask != 0 && e.Flag&^m.FlagMask != 0) {
			return flagError(e, fmt.Sprintf("flag bits contradict mask %d", m.FlagMask))
		}
	}
	return nil
}

func flagError(e domain.Event, reason string) error {
	return &errs.Error{Code: errs.EFlag, Branch: e.SourceBranch, Path: e.SourcePath + ".fl", Reason: reason, Channel: e.Channel, EventID: e.ID, Frame: e.Start}
}

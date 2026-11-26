package locklab

import "sort"

// HeldLock is the normalized lock state used by both arbitration and
// checkpoints.
type HeldLock struct {
	TxID         string      `json:"tx_id"`
	Resource     string      `json:"resource"`
	Granularity  Granularity `json:"granularity"`
	Table        string      `json:"table"`
	ItemID       string      `json:"item_id,omitempty"`
	Mode         LockMode    `json:"mode"`
	AcquiredStep int         `json:"acquired_step"`
	Transient    bool        `json:"transient,omitempty"`
}

// HeldLocksConflict implements same-level compatibility and the table/row
// hierarchy rules. Intention modes do not directly block descendant row locks;
// S/SIX/X table locks do.
func HeldLocksConflict(a, b HeldLock) bool {
	if a.TxID == b.TxID {
		return false
	}
	if a.Granularity == TableGranularity && b.Granularity == TableGranularity {
		if a.Table != b.Table {
			return false
		}
		return !ModesCompatible(a.Mode, b.Mode)
	}
	if a.Granularity == RowGranularity && b.Granularity == RowGranularity {
		if a.ItemID != b.ItemID {
			return false
		}
		return !ModesCompatible(a.Mode, b.Mode)
	}

	tableLock, rowLock := a, b
	if a.Granularity == RowGranularity {
		tableLock, rowLock = b, a
	}
	if tableLock.Table != rowLock.Table {
		return false
	}
	switch tableLock.Mode {
	case ModeX:
		return true
	case ModeS, ModeSIX:
		return rowLock.Mode == ModeX || rowLock.Mode == ModeSIX
	default:
		return false
	}
}

func conflictingLocks(txID string, requested HeldLock, locks map[string]HeldLock) []string {
	opponents := make(map[string]struct{})
	for _, held := range locks {
		if held.TxID == txID {
			continue
		}
		if HeldLocksConflict(requested, held) {
			opponents[held.TxID] = struct{}{}
		}
	}
	out := make([]string, 0, len(opponents))
	for id := range opponents {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func sortedLockKeys(locks map[string]HeldLock, txID string) []string {
	keys := make([]string, 0)
	for key, lock := range locks {
		if txID == "" || lock.TxID == txID {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

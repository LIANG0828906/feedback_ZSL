package recovery

import (
	"fmt"

	"enginelab.local/internal/model"
)

func ValidateSequence(p *model.Project) error {
	seenRecord := map[string]string{}
	seenLSN := map[uint64]string{}
	var globalLSN uint64
	haveGlobal := false

	for _, seg := range p.SortedSegments() {
		locSeg := fmt.Sprintf("segment:%s", seg.ID)
		if len(seg.Records) > 0 && seg.Records[0].LSN < seg.BaseLSN {
			return model.NewError(model.EBaseConflict, locSeg+".base_lsn",
				"first lsn %d is before base lsn %d", seg.Records[0].LSN, seg.BaseLSN)
		}
		var prevLSN uint64
		havePrev := false
		for i, rec := range seg.Records {
			locator := fmt.Sprintf("%s.record[%d](%s)", locSeg, i, rec.ID)
			if rec.ID == "" {
				return model.NewError(model.EFieldRequired, locator+".id", "record id is required")
			}
			if prior, ok := seenRecord[rec.ID]; ok {
				return model.NewError(model.EDupRecord, locator, "record id %q already seen in %s", rec.ID, prior).
					WithDetails(map[string]any{"record_id": rec.ID, "first_locator": prior})
			}
			seenRecord[rec.ID] = locator
			if prior, ok := seenLSN[rec.LSN]; ok {
				return model.NewError(model.EDupRecord, locator+".lsn", "lsn %d already used by %s", rec.LSN, prior).
					WithDetails(map[string]any{"lsn": rec.LSN, "first_locator": prior})
			}
			seenLSN[rec.LSN] = locator
			if rec.LSN < seg.BaseLSN {
				return model.NewError(model.EBaseConflict, locator+".lsn", "lsn %d is before segment base %d", rec.LSN, seg.BaseLSN)
			}
			if expectedOffset := rec.LSN - seg.BaseLSN; rec.Offset != expectedOffset {
				return model.NewError(model.EBaseConflict, locator+".offset",
					"offset %d conflicts with lsn/base; expected offset %d", rec.Offset, expectedOffset).
					WithDetails(map[string]any{"lsn": rec.LSN, "base_lsn": seg.BaseLSN, "offset": rec.Offset, "expected_offset": expectedOffset})
			}
			if havePrev && rec.LSN <= prevLSN {
				return model.NewError(model.EOrder, locator+".lsn",
					"sequence order violation: lsn %d follows %d", rec.LSN, prevLSN).
					WithDetails(map[string]any{"previous_lsn": prevLSN, "current_lsn": rec.LSN})
			}
			prevLSN = rec.LSN
			havePrev = true
			if haveGlobal && rec.LSN < globalLSN {
				return model.NewError(model.EOrder, locator, "global record order violation: lsn %d after max %d", rec.LSN, globalLSN)
			}
			globalLSN = rec.LSN
			haveGlobal = true
		}
	}
	return nil
}

func CompareVersionProjects(versions map[model.SchemaVersion]*model.Project) error {
	var canonical *model.Project
	var canonicalVersion model.SchemaVersion
	for _, v := range []model.SchemaVersion{model.SchemaV1, model.SchemaV2, model.SchemaV3} {
		if p, ok := versions[v]; ok {
			canonical = p
			canonicalVersion = v
			break
		}
	}
	if canonical == nil {
		return model.NewError(model.EFieldRequired, "$.versions", "no loaded project versions")
	}
	for version, p := range versions {
		if version == canonicalVersion {
			continue
		}
		if err := compareProjects(canonical, p, canonicalVersion, version); err != nil {
			return err
		}
	}
	return nil
}

func compareProjects(a, b *model.Project, va, vb model.SchemaVersion) error {
	if len(a.Segments) != len(b.Segments) {
		return model.NewError(model.EBaseConflict, "$.segments", "%s has %d segments, %s has %d", va, len(a.Segments), vb, len(b.Segments))
	}
	for i := range a.Segments {
		as := a.SortedSegments()[i]
		bs := b.SortedSegments()[i]
		loc := fmt.Sprintf("segment:%s vs %s", as.ID, bs.ID)
		if as.ID != bs.ID {
			return model.NewError(model.EBaseConflict, loc, "segment id/order mismatch: %s != %s", as.ID, bs.ID)
		}
		if as.BaseLSN != bs.BaseLSN {
			return model.NewError(model.EBaseConflict, loc+".base_lsn", "base lsn %d != %d", as.BaseLSN, bs.BaseLSN).
				WithDetails(map[string]any{"schema_a": va, "schema_b": vb, "base_a": as.BaseLSN, "base_b": bs.BaseLSN})
		}
		if len(as.Records) != len(bs.Records) {
			return model.NewError(model.EBaseConflict, loc+".records", "record count %d != %d", len(as.Records), len(bs.Records))
		}
		for j := range as.Records {
			ar := as.Records[j]
			br := bs.Records[j]
			rloc := fmt.Sprintf("%s.record[%d]", loc, j)
			if ar.ID != br.ID {
				return model.NewError(model.EBaseConflict, rloc+".id", "record id %s != %s", ar.ID, br.ID)
			}
			if ar.LSN != br.LSN {
				return model.NewError(model.EBaseConflict, rloc+".lsn",
					"cross-version sequence base conflict: normalized lsn %d != %d", ar.LSN, br.LSN).
					WithDetails(map[string]any{"record_id": ar.ID, "lsn_a": ar.LSN, "lsn_b": br.LSN, "schema_a": va, "schema_b": vb})
			}
			if ar.Op != br.Op || ar.Key != br.Key || ar.Value != br.Value || ar.Version != br.Version {
				return model.NewError(model.EFieldAmbiguous, rloc, "record payload differs across schema versions")
			}
		}
		if fmt.Sprint(as.PredecessorIDs) != fmt.Sprint(bs.PredecessorIDs) {
			return model.NewError(model.EDepMissing, loc+".predecessor_ids", "dependencies differ: %v != %v", as.PredecessorIDs, bs.PredecessorIDs)
		}
	}
	if (a.LatestCheckpoint == nil) != (b.LatestCheckpoint == nil) {
		return model.NewError(model.EBaseConflict, "$.checkpoint", "checkpoint presence differs across versions")
	}
	if a.LatestCheckpoint != nil && a.LatestCheckpoint.LSN != b.LatestCheckpoint.LSN {
		return model.NewError(model.EBaseConflict, "$.checkpoint.lsn", "checkpoint lsn %d != %d", a.LatestCheckpoint.LSN, b.LatestCheckpoint.LSN)
	}
	return nil
}

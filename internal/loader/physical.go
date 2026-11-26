package loader

import (
	"bytes"
	"encoding/json"
	"fmt"

	"enginelab.local/internal/model"
)

type segmentFileHeader struct {
	Kind        string `json:"kind"`
	SegmentID   string `json:"segment_id"`
	RecordCount int    `json:"record_count"`
	BaseLSN     uint64 `json:"base_lsn"`
}

type segmentFileEnvelope struct {
	Kind string `json:"kind"`
	model.Record
}

func EncodeSegmentFile(seg *model.Segment) ([]byte, error) {
	header := segmentFileHeader{
		Kind:        "segment-header",
		SegmentID:   seg.ID,
		RecordCount: len(seg.Records),
		BaseLSN:     seg.BaseLSN,
	}
	var buf bytes.Buffer
	hb, err := json.Marshal(header)
	if err != nil {
		return nil, model.WrapError(model.EFieldRequired, seg.ID+".header", err)
	}
	buf.Write(hb)
	buf.WriteByte('\n')
	for _, rec := range seg.Records {
		line, err := json.Marshal(rec)
		if err != nil {
			return nil, model.WrapError(model.EFieldRequired, rec.ID, err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	return buf.Bytes(), nil
}

func ParseSegmentFile(id string, data []byte) (*model.Segment, error) {
	seg := &model.Segment{ID: id, Integrity: model.IntegrityComplete}
	lines := bytes.Split(data, []byte("\n"))
	recordLine := 0
	lastNonEmpty := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if len(bytes.TrimSpace(lines[i])) > 0 {
			lastNonEmpty = i
			break
		}
	}
	for lineNo, raw := range lines {
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 {
			continue
		}
		var head struct {
			Kind      string `json:"kind"`
			SegmentID string `json:"segment_id"`
		}
		if err := json.Unmarshal(trimmed, &head); err == nil && head.Kind == "segment-header" {
			var header segmentFileHeader
			if err := json.Unmarshal(trimmed, &header); err != nil {
				return nil, model.WrapError(model.EFieldRequired, fmt.Sprintf("%s:line:%d", id, lineNo+1), err)
			}
			if header.SegmentID != "" {
				seg.ID = header.SegmentID
			}
			seg.BaseLSN = header.BaseLSN
			seg.RawRecordCount = header.RecordCount
			continue
		}
		var rec model.Record
		if err := json.Unmarshal(trimmed, &rec); err != nil {
			locator := fmt.Sprintf("%s:line:%d", id, lineNo+1)
			if lineNo == lastNonEmpty {
				seg.Integrity = model.IntegrityDegraded
				seg.Diagnostics = append(seg.Diagnostics, model.Diagnostic{
					Code:     model.ERecordIncomplete,
					Severity: "warning",
					Locator:  locator,
					Message:  "last record is incomplete and was skipped",
					Details:  map[string]any{"complete_records": recordLine},
				})
				break
			}
			return nil, model.WrapError(model.ERecordIncomplete, locator, err)
		}
		if rec.SegmentID == "" {
			rec.SegmentID = seg.ID
		}
		rec.Index = recordLine
		if !rec.Complete {
			seg.Integrity = model.IntegrityDegraded
			seg.Diagnostics = append(seg.Diagnostics, model.Diagnostic{
				Code:     model.ERecordIncomplete,
				Severity: "warning",
				Locator:  fmt.Sprintf("%s:record:%s", id, rec.ID),
				Message:  "record is explicitly marked incomplete and was skipped",
			})
			break
		}
		seg.Records = append(seg.Records, &rec)
		recordLine++
	}
	seg.CompleteRecords = len(seg.Records)
	if seg.RawRecordCount == 0 {
		seg.RawRecordCount = len(seg.Records)
	}
	if seg.RawRecordCount > len(seg.Records) && seg.Integrity == model.IntegrityComplete {
		seg.Integrity = model.IntegrityDegraded
		seg.Diagnostics = append(seg.Diagnostics, model.Diagnostic{
			Code:     model.ESegmentTruncated,
			Severity: "warning",
			Locator:  id,
			Message:  "segment file is truncated before header record_count",
			Details: map[string]any{
				"expected_records": seg.RawRecordCount,
				"complete_records": seg.CompleteRecords,
			},
		})
	}
	if len(seg.Records) > 0 {
		seg.FirstLSN = seg.Records[0].LSN
		seg.LastLSN = seg.Records[len(seg.Records)-1].LSN
	}
	return seg, nil
}

func TruncateSegmentFileToRecords(data []byte, records int) ([]byte, error) {
	lines := bytes.Split(data, []byte("\n"))
	var out [][]byte
	keptRecords := 0
	for _, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var head struct {
			Kind string `json:"kind"`
		}
		_ = json.Unmarshal(line, &head)
		if head.Kind == "segment-header" {
			out = append(out, line)
			continue
		}
		if keptRecords >= records {
			break
		}
		out = append(out, line)
		keptRecords++
	}
	return append(bytes.Join(out, []byte("\n")), '\n'), nil
}

func CorruptLastRecordByte(data []byte) []byte {
	trimmed := bytes.TrimRight(data, "\n")
	if len(trimmed) < 12 {
		return append(trimmed, []byte("{\"op\":\"pu")...)
	}
	cut := len(trimmed) - 8
	return append(trimmed[:cut], []byte(`{"op":"pu`)...)
}

func EncodeCheckpointFile(cp *model.Checkpoint) ([]byte, error) {
	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return nil, model.WrapError(model.ECPCorrupt, cp.ID, err)
	}
	return data, nil
}

func ParseCheckpointFile(data []byte) (*model.Checkpoint, error) {
	var cp model.Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, model.WrapError(model.ECPCorrupt, "$.checkpoint_file", err)
	}
	if cp.ID == "" || cp.SegmentID == "" {
		return nil, model.NewError(model.ECPCorrupt, "$.checkpoint_file", "checkpoint id and segment are required")
	}
	return &cp, nil
}

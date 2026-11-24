package verifier

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"mediaforge/internal/domain"
	"mediaforge/internal/errs"
	"mediaforge/internal/mfxfmt"
	"mediaforge/internal/state"
	"mediaforge/internal/store"
)

type VariantResult struct {
	ID             string   `json:"id"`
	Ordinal        int      `json:"ordinal"`
	PixelCount     int64    `json:"pixelCount"`
	AudioSamples   int64    `json:"audioSamples"`
	MetadataFields []string `json:"metadataFields"`
	Fingerprint    string   `json:"fingerprint"`
	SizeBytes      int64    `json:"sizeBytes"`
	MainChainMatch bool     `json:"mainChainMatch"`
	TruthMatch     bool     `json:"truthMatch"`
}

type Report struct {
	Checked      int             `json:"checked"`
	AllMatch     bool            `json:"allMatch"`
	Variants     []VariantResult `json:"variants"`
	FirstFailure *Failure        `json:"firstFailure,omitempty"`
}

type Failure struct {
	Variant  string `json:"variant"`
	Item     string `json:"item"`
	Actual   any    `json:"actual"`
	Expected any    `json:"expected"`
}

type Verifier struct {
	Store *store.Store
}

func (v *Verifier) Verify(ctx context.Context) (*Report, error) {
	if v.Store == nil {
		return nil, errs.New(errs.EVerify, "verifier requires an open SQLite checkpoint")
	}
	records, err := v.Store.Variants(ctx)
	if err != nil {
		return nil, errs.Wrap(errs.EVerify, "read persisted variants", err)
	}
	report := &Report{AllMatch: true}
	for _, rec := range records {
		if rec.State != state.Committed {
			continue
		}
		res, failure, err := verifyRecord(rec)
		if err != nil {
			return report, err
		}
		report.Checked++
		report.Variants = append(report.Variants, res)
		if failure != nil {
			report.AllMatch = false
			report.FirstFailure = failure
			return report, errs.Diff(errs.Mismatch{
				Variant: failure.Variant, Artifact: "mfx", Field: failure.Item,
				Actual: failure.Actual, Expected: failure.Expected,
			}, "independent verification found a persisted artifact mismatch")
		}
	}
	if report.Checked == 0 {
		return report, errs.New(errs.EVerify, "no committed variants are available in SQLite")
	}
	return report, nil
}

func verifyRecord(rec store.VariantRecord) (VariantResult, *Failure, error) {
	res := VariantResult{ID: rec.ID, Ordinal: rec.Ordinal}
	if len(rec.OutputBlob) == 0 {
		return res, nil, &errs.Error{
			Code: errs.EVerify, Message: "committed output blob is missing", Variant: rec.ID,
		}
	}
	pkg, err := mfxfmt.Parse(rec.OutputBlob)
	if err != nil {
		return res, nil, errs.Wrap(errs.EVerify, "parse persisted package: "+rec.ID, err)
	}
	var truth domain.TruthVariant
	if rec.TruthJSON == "" {
		return res, nil, &errs.Error{
			Code: errs.EVerify, Message: "persisted truth snapshot is missing", Variant: rec.ID,
		}
	}
	if err := json.Unmarshal([]byte(rec.TruthJSON), &truth); err != nil {
		return res, nil, errs.Wrap(errs.EVerify, "decode persisted truth: "+rec.ID, err)
	}
	var summary map[string]any
	if rec.SummaryJSON != "" {
		if err := json.Unmarshal([]byte(rec.SummaryJSON), &summary); err != nil {
			return res, nil, errs.Wrap(errs.EVerify, "decode main-chain summary: "+rec.ID, err)
		}
	}
	actualHash := mfxfmt.SHA256(rec.OutputBlob)
	res.PixelCount = pkg.Header.PixelCount
	res.AudioSamples = pkg.Header.AudioSamples
	res.MetadataFields = append([]string(nil), pkg.Header.MetadataFields...)
	res.Fingerprint = actualHash
	res.SizeBytes = int64(len(rec.OutputBlob))
	res.MainChainMatch = true
	res.TruthMatch = true
	if failure := compareMainChain(rec, pkg, actualHash, summary); failure != nil {
		res.MainChainMatch = false
		return res, failure, nil
	}
	if failure := compareTruth(pkg, actualHash, truth); failure != nil {
		res.TruthMatch = false
		return res, failure, nil
	}
	return res, nil, nil
}

func compareMainChain(rec store.VariantRecord, pkg mfxfmt.Package, actualHash string,
	summary map[string]any) *Failure {
	if rec.OutputSHA256 != actualHash {
		return &Failure{Variant: rec.ID, Item: "main.outputSha256", Actual: actualHash, Expected: rec.OutputSHA256}
	}
	if rec.OutputSize != int64(len(rec.OutputBlob)) {
		return &Failure{Variant: rec.ID, Item: "main.outputSize", Actual: len(rec.OutputBlob), Expected: rec.OutputSize}
	}
	if summary == nil {
		return &Failure{Variant: rec.ID, Item: "main.summary", Actual: nil, Expected: "present"}
	}
	if getInt(summary, "width") != pkg.Header.Width {
		return &Failure{Variant: rec.ID, Item: "main.width", Actual: pkg.Header.Width, Expected: getInt(summary, "width")}
	}
	if getInt(summary, "height") != pkg.Header.Height {
		return &Failure{Variant: rec.ID, Item: "main.height", Actual: pkg.Header.Height, Expected: getInt(summary, "height")}
	}
	if getInt(summary, "channels") != pkg.Header.ImageChannels {
		return &Failure{Variant: rec.ID, Item: "main.channels", Actual: pkg.Header.ImageChannels, Expected: getInt(summary, "channels")}
	}
	if getInt64(summary, "audioSamples") != pkg.Header.AudioSamples {
		return &Failure{Variant: rec.ID, Item: "main.audioSamples", Actual: pkg.Header.AudioSamples, Expected: getInt64(summary, "audioSamples")}
	}
	summaryFields := getStrings(summary, "metadataFields")
	if !equalStrings(summaryFields, pkg.Header.MetadataFields) {
		return &Failure{Variant: rec.ID, Item: "main.metadataFields", Actual: pkg.Header.MetadataFields, Expected: summaryFields}
	}
	return nil
}

func compareTruth(pkg mfxfmt.Package, actualHash string, truth domain.TruthVariant) *Failure {
	if pkg.Header.Width != truth.Width {
		return &Failure{Variant: truth.ID, Item: "width", Actual: pkg.Header.Width, Expected: truth.Width}
	}
	if pkg.Header.Height != truth.Height {
		return &Failure{Variant: truth.ID, Item: "height", Actual: pkg.Header.Height, Expected: truth.Height}
	}
	if pkg.Header.ImageChannels != truth.Channels {
		return &Failure{Variant: truth.ID, Item: "channels", Actual: pkg.Header.ImageChannels, Expected: truth.Channels}
	}
	if pkg.Header.PixelCount != truth.PixelCount {
		return &Failure{Variant: truth.ID, Item: "pixelCount", Actual: pkg.Header.PixelCount, Expected: truth.PixelCount}
	}
	if pkg.Header.AudioSamples != truth.AudioSamples {
		return &Failure{Variant: truth.ID, Item: "audioSamples", Actual: pkg.Header.AudioSamples, Expected: truth.AudioSamples}
	}
	if !equalStrings(pkg.Header.MetadataFields, truth.MetadataFields) {
		return &Failure{Variant: truth.ID, Item: "metadataFields", Actual: pkg.Header.MetadataFields, Expected: truth.MetadataFields}
	}
	if actualHash != truth.Fingerprint {
		return &Failure{Variant: truth.ID, Item: "fingerprint", Actual: actualHash, Expected: truth.Fingerprint}
	}
	if int64(pkg.TotalSize) != truth.SizeBytes {
		return &Failure{Variant: truth.ID, Item: "sizeBytes", Actual: pkg.TotalSize, Expected: truth.SizeBytes}
	}
	return nil
}

func getInt(m map[string]any, key string) int {
	v, _ := m[key].(float64)
	return int(v)
}

func getInt64(m map[string]any, key string) int64 {
	v, _ := m[key].(float64)
	return int64(v)
}

func getStrings(m map[string]any, key string) []string {
	raw, _ := m[key].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	aa := append([]string(nil), a...)
	bb := append([]string(nil), b...)
	sort.Strings(aa)
	sort.Strings(bb)
	if len(aa) != len(bb) {
		return false
	}
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}
	return true
}

func Describe(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("%s", err)
}

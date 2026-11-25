package schema

import (
	"fmt"

	"presentkernel/internal/domain"
	"presentkernel/internal/quant"
)

var v1CodeToID = map[string]int{
	"SIL": 0, "MBP": 1, "FV": 2, "TDN": 3, "SZ": 4, "KG": 5,
	"L": 6, "A": 10, "E": 11, "I": 12, "O": 13, "U": 14, "TR": 99,
}

var v1IDToCode = func() map[int]string {
	out := map[int]string{}
	for code, id := range v1CodeToID {
		out[id] = code
	}
	return out
}()

func v1CodebookFromStandard() []V1Viseme {
	out := make([]V1Viseme, 0, len(domain.StandardCategories()))
	for _, c := range domain.StandardCategories() {
		out = append(out, V1Viseme{VisemeID: v1CodeToID[c.Code], StandardCode: c.Code, Family: c.Family, Label: c.Label})
	}
	return out
}

func v2GroupsFromStandard() []V2MouthGroup {
	families := []string{}
	byFamily := map[string][]V2Shape{}
	for _, c := range domain.StandardCategories() {
		if _, ok := byFamily[c.Family]; !ok {
			families = append(families, c.Family)
		}
		byFamily[c.Family] = append(byFamily[c.Family], V2Shape{ShapeID: "SHAPE_" + c.Code, StandardCode: c.Code, Label: c.Label})
	}
	out := make([]V2MouthGroup, 0, len(families))
	for _, f := range families {
		out = append(out, V2MouthGroup{FamilyName: f, Shapes: byFamily[f]})
	}
	return out
}

func secondsFromSamples(field string, samples int64, rate int) (string, error) {
	ns, _, err := quant.SamplesToNanoseconds(field, samples, rate, quant.RoundFloor)
	if err != nil {
		return "", err
	}
	whole := ns / int64(1_000_000_000)
	frac := ns % int64(1_000_000_000)
	if frac < 0 {
		frac = -frac
	}
	return fmt.Sprintf("%d.%09d", whole, frac), nil
}

func msFromSamples(field string, samples int64, rate int) (int64, error) {
	ms, _, err := quant.SamplesToMs(field, samples, rate, quant.RoundHalfUp)
	return ms, err
}

func framesFromSamples(field string, samples int64, rate, fps int, mode quant.RoundingMode, trace *domain.ConversionTrace) (int, error) {
	frame, rem, err := quant.SamplesToFrames(field, samples, rate, fps, mode)
	if err != nil {
		return 0, err
	}
	if rem.Remainder != 0 {
		trace.Remainders = append(trace.Remainders, rem)
		addField(trace, field, "samples", "frame", "samples_to_frame", samples, frame, true, rem.Remainder)
	}
	return frame, nil
}

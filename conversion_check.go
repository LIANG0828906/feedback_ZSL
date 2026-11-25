package ocrflow

import "fmt"

type RoundTripItem struct {
	ID                    string `json:"id"`
	OriginalThresholdBPS  int    `json:"original_threshold_bps"`
	RoundTripThresholdBPS int    `json:"round_trip_threshold_bps"`
	ThresholdDeltaBPS     int    `json:"threshold_delta_bps"`
	OriginalBox           Quad   `json:"original_box"`
	RoundTripBox          Quad   `json:"round_trip_box"`
	MaxCoordinateDeltaPx  int    `json:"max_coordinate_delta_px"`
	OriginalCharCount     int    `json:"original_char_count"`
	RoundTripCharCount    int    `json:"round_trip_char_count"`
	OK                    bool   `json:"ok"`
}

type ConversionReport struct {
	ThresholdToleranceBPS int             `json:"threshold_tolerance_bps"`
	CoordinateTolerancePx int             `json:"coordinate_tolerance_px"`
	Items                 []RoundTripItem `json:"items"`
	OK                    bool            `json:"ok"`
}

// VerifyConversionRoundTrip performs high(v3) -> low(v1) -> high(v3) mapping
// using only integer schema conversion routines.
func VerifyConversionRoundTrip(r *Recipe) (*ConversionReport, error) {
	v1Data, err := EncodeRecipeDocument(r, SchemaV1)
	if err != nil {
		return nil, err
	}
	r1, err := LoadRecipeDocument(v1Data)
	if err != nil {
		return nil, err
	}
	v3Data, err := EncodeRecipeDocument(r1, SchemaV3)
	if err != nil {
		return nil, err
	}
	r3, err := LoadRecipeDocument(v3Data)
	if err != nil {
		return nil, err
	}
	byID := map[string]RecipeItem{}
	for _, item := range r3.Items {
		byID[item.ID] = item
	}
	rep := &ConversionReport{ThresholdToleranceBPS: ThresholdResidualBP, CoordinateTolerancePx: CoordResidualPx, OK: true}
	for _, original := range r.Items {
		rt, ok := byID[original.ID]
		if !ok {
			return nil, fmt.Errorf("round-trip item %s missing", original.ID)
		}
		thresholdDelta := absInt(original.ThresholdBPS - rt.ThresholdBPS)
		maxCoord := 0
		for i := 0; i < 4; i++ {
			maxCoord = maxInt(maxCoord, absInt(original.Box[i].X-rt.Box[i].X))
			maxCoord = maxInt(maxCoord, absInt(original.Box[i].Y-rt.Box[i].Y))
		}
		itemOK := thresholdDelta <= ThresholdResidualBP && maxCoord <= CoordResidualPx && original.CharCount == rt.CharCount
		rep.Items = append(rep.Items, RoundTripItem{
			ID: original.ID, OriginalThresholdBPS: original.ThresholdBPS, RoundTripThresholdBPS: rt.ThresholdBPS,
			ThresholdDeltaBPS: thresholdDelta, OriginalBox: original.Box, RoundTripBox: rt.Box,
			MaxCoordinateDeltaPx: maxCoord, OriginalCharCount: original.CharCount, RoundTripCharCount: rt.CharCount, OK: itemOK,
		})
		if !itemOK {
			rep.OK = false
		}
	}
	return rep, nil
}

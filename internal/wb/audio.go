package wb

type CurveAccumulator struct {
	spec        PrecisionSpec
	params      RoomParams
	initialSPL  Fixed
	decayStepDB Fixed
	decayDB     Fixed
	sampleIndex int
}

func NewCurveAccumulator(s *Sample, path *PathResult) *CurveAccumulator {
	source, _ := FindSource(s, path.SourceID)
	room, _ := FindRoom(s, path.ListenerRoom)
	spec := s.Precision
	params := room.Params
	initialSPL := source.PowerDB + params.GainDB -
		Fixed(path.Length)*spec.PathAttenPerUnit -
		path.OcclusionDB
	return &CurveAccumulator{
		spec:        spec,
		params:      params,
		initialSPL:  initialSPL,
		decayStepDB: Fixed(60000 * spec.DeltaTMS / params.RT60MS),
	}
}

func (a *CurveAccumulator) InitialSPL() Fixed { return a.initialSPL }

func (a *CurveAccumulator) Next() CurveSample {
	tMS := int64(a.sampleIndex) * a.spec.DeltaTMS
	level := a.initialSPL - a.decayDB + earlyReflectionBoost(a.spec, a.params, tMS)
	a.decayDB += a.decayStepDB
	a.sampleIndex++
	return CurveSample{TMS: tMS, LevelDB: level}
}

func SynthesizePair(s *Sample, path *PathResult) ([]CurveSample, Fixed) {
	spec := s.Precision
	accumulator := NewCurveAccumulator(s, path)

	samples := make([]CurveSample, 0, spec.SampleCount)
	for i := 0; i < spec.SampleCount; i++ {
		samples = append(samples, accumulator.Next())
	}
	return samples, accumulator.InitialSPL()
}

func earlyReflectionBoost(spec PrecisionSpec, params RoomParams, tMS int64) Fixed {
	if spec.EarlyWindowMS <= 0 || tMS >= spec.EarlyWindowMS || params.EarlyReflectionDensity == 0 {
		return 0
	}
	fadeMS := spec.EarlyWindowMS - tMS
	numerator := int64(params.EarlyReflectionDensity) *
		int64(spec.EarlyBoostMaxDB) * fadeMS
	denominator := FixedScale * spec.EarlyWindowMS
	return Fixed(numerator / denominator)
}

func BuildTruth(s *Sample) (*Truth, *APIError) {
	g := BuildGraph(s)
	truth := &Truth{ListenerRooms: map[string]string{}}
	for _, l := range s.Listeners {
		roomID, apiErr := RoomAtPoint(s, Point{X: l.X, Y: l.Y}, l.ID, "listener")
		if apiErr != nil {
			return nil, apiErr
		}
		truth.ListenerRooms[l.ID] = roomID
	}
	for _, src := range s.Sources {
		for _, l := range s.Listeners {
			path, apiErr := SolvePairPath(s, g, src.ID, l.ID)
			if apiErr != nil {
				return nil, apiErr
			}
			samples, spl := SynthesizePair(s, path)
			values := make([]Fixed, 0, len(samples))
			for _, sample := range samples {
				values = append(values, sample.LevelDB)
			}
			truth.Paths = append(truth.Paths, TruthPath{
				SourceID:       src.ID,
				ListenerID:     l.ID,
				RoomChain:      append([]string(nil), path.RoomChain...),
				DoorChain:      append([]string(nil), path.DoorChain...),
				Length:         path.Length,
				OcclusionCount: path.OcclusionCount,
				OcclusionDB:    path.OcclusionDB,
				ExpectedSPLDB:  spl,
				DecaySamplesDB: values,
			})
		}
	}
	return truth, nil
}

func BuildAnalyticTruth(s *Sample) Truth {
	truth, err := BuildTruth(s)
	if err != nil {
		panic(err.Error())
	}
	return *truth
}

func FindTruthPath(t Truth, sourceID, listenerID string) (TruthPath, bool) {
	for _, tp := range t.Paths {
		if tp.SourceID == sourceID && tp.ListenerID == listenerID {
			return tp, true
		}
	}
	return TruthPath{}, false
}

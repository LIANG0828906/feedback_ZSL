package schema

// V1 uses snake_case field names, decimal seconds, permille fixed point and a
// flat numeric viseme codebook. It has no transition policy/override field.
type V1Document struct {
	SchemaName            string     `json:"schema_name"`
	SchemaVersion         string     `json:"schema_version"`
	ProjectID             string     `json:"project_id"`
	FixedSeed             int64      `json:"fixed_seed"`
	AudioFormat           *V1Audio   `json:"audio_format"`
	RenderProfile         *V1Render  `json:"render_profile"`
	DefaultVolumePermille *int       `json:"default_volume_permille"`
	VisemeCodebook        []V1Viseme `json:"viseme_codebook"`
	DialogueCues          []V1Cue    `json:"dialogue_cues"`
}

type V1Audio struct {
	SampleRateHz int `json:"sample_rate_hz"`
}

type V1Render struct {
	FramesPerSecond       int    `json:"frames_per_second"`
	MinimumReadableFrames *int   `json:"minimum_readable_frames"`
	RoundingDirection     string `json:"rounding_direction"`
}

type V1Viseme struct {
	VisemeID     int    `json:"viseme_id"`
	StandardCode string `json:"standard_code"`
	Family       string `json:"family"`
	Label        string `json:"label"`
}

type V1Cue struct {
	CueID       string   `json:"cue_id"`
	SpeakerName string   `json:"speaker_name"`
	Lines       []V1Line `json:"lines"`
}

type V1Line struct {
	LineID           string             `json:"line_id"`
	TextContent      string             `json:"text_content"`
	DurationSeconds  string             `json:"duration_seconds"`
	CharacterWeights []V1WeightPermille `json:"character_weights"`
	PhonemeTrack     *V1PhonemeTrack    `json:"phoneme_track"`
}

type V1WeightPermille struct {
	Character      string `json:"character"`
	WeightPermille int    `json:"weight_permille"`
}

type V1PhonemeTrack struct {
	Encoding string      `json:"encoding"`
	Phonemes []V1Phoneme `json:"phonemes"`
}

type V1Phoneme struct {
	OrderIndex   int    `json:"order_index"`
	Symbol       string `json:"symbol"`
	VisemeID     int    `json:"viseme_id"`
	StartSeconds string `json:"start_seconds"`
	EndSeconds   string `json:"end_seconds"`
}

// V2 uses camelCase, integer milliseconds, permyriad/basis-point weights and
// a codebook grouped by mouth-shape family. A tolerance field is optional.
type V2Document struct {
	SchemaVersion   string        `json:"schemaVersion"`
	ProjectID       string        `json:"projectId"`
	Seed            int64         `json:"seed"`
	Media           *V2Media      `json:"media"`
	Timing          *V2Timing     `json:"timing"`
	DefaultVolumeBp *int          `json:"defaultVolumeBp"`
	MouthTable      *V2MouthTable `json:"mouthTable"`
	Dialogues       []V2Dialogue  `json:"dialogues"`
}

type V2Media struct {
	SampleRateHz int `json:"sampleRateHz"`
}

type V2Timing struct {
	FrameRate                 int    `json:"frameRate"`
	MinimumReadableFrames     *int   `json:"minimumReadableFrames"`
	Rounding                  string `json:"rounding"`
	TransitionToleranceFrames *int   `json:"transitionToleranceFrames"`
}

type V2MouthTable struct {
	Encoding string         `json:"encoding"`
	Groups   []V2MouthGroup `json:"groups"`
}

type V2MouthGroup struct {
	FamilyName string    `json:"familyName"`
	Shapes     []V2Shape `json:"shapes"`
}

type V2Shape struct {
	ShapeID      string `json:"shapeId"`
	StandardCode string `json:"standardCode"`
	Label        string `json:"label"`
}

type V2Dialogue struct {
	ID          string       `json:"id"`
	SpeakerName string       `json:"speakerName"`
	Fragments   []V2Fragment `json:"fragments"`
}

type V2Fragment struct {
	FragmentID                string       `json:"fragmentId"`
	Text                      string       `json:"text"`
	DurationMs                int64        `json:"durationMs"`
	CharacterWeightsBp        []V2WeightBp `json:"characterWeightsBp"`
	Phonemes                  []V2Phoneme  `json:"phonemes"`
	TransitionToleranceFrames *int         `json:"transitionToleranceFrames,omitempty"`
}

type V2WeightBp struct {
	Grapheme string `json:"grapheme"`
	WeightBp int    `json:"weightBp"`
}

type V2Phoneme struct {
	Order        int    `json:"order"`
	Symbol       string `json:"symbol"`
	MouthShapeID string `json:"mouthShapeId"`
	StartMs      int64  `json:"startMs"`
	EndMs        int64  `json:"endMs"`
}

// V3 nests the project, uses milliseconds plus exact sample points, explicit
// permyriad scales, a phoneme-to-category mapping and optional transitions.
type V3Document struct {
	SchemaVersion       string    `json:"schemaVersion"`
	PresentationProject V3Project `json:"presentationProject"`
}

type V3Project struct {
	ID                string          `json:"id"`
	DeterministicSeed int64           `json:"deterministicSeed"`
	TimingProfile     V3TimingProfile `json:"timingProfile"`
	OutputScales      V3Scales        `json:"outputScales"`
	LipMapping        V3LipMapping    `json:"lipMapping"`
	Dialogues         []V3Dialogue    `json:"dialogues"`
}

type V3TimingProfile struct {
	SampleRateHz              int    `json:"sampleRateHz"`
	FramesPerSecond           int    `json:"framesPerSecond"`
	MinimumReadableFrames     int    `json:"minimumReadableFrames"`
	Quantization              string `json:"quantization"`
	TransitionToleranceFrames int    `json:"transitionToleranceFrames"`
}

type V3Scales struct {
	WeightUnit    string `json:"weightUnit"`
	VolumeUnit    string `json:"volumeUnit"`
	DefaultVolume int    `json:"defaultVolume"`
}

type V3LipMapping struct {
	Encoding          string            `json:"encoding"`
	Categories        []V3Category      `json:"categories"`
	PhonemeToCategory map[string]string `json:"phonemeToCategory"`
}

type V3Category struct {
	CategoryID string `json:"categoryId"`
	Family     string `json:"family"`
	Label      string `json:"label"`
}

type V3Dialogue struct {
	DialogueID string        `json:"dialogueId"`
	VoiceName  string        `json:"voiceName"`
	Partitions []V3Partition `json:"partitions"`
}

type V3Partition struct {
	PartitionID     string         `json:"partitionId"`
	SpokenText      string         `json:"spokenText"`
	DurationMs      int64          `json:"durationMs"`
	DurationSamples int64          `json:"durationSamples"`
	Weights         []V3Weight     `json:"weights"`
	Phonemes        []V3Phoneme    `json:"phonemes"`
	Transitions     []V3Transition `json:"transitions,omitempty"`
}

type V3Weight struct {
	Grapheme        string `json:"grapheme"`
	WeightPermyriad int    `json:"weightPermyriad"`
}

type V3Phoneme struct {
	Sequence    int    `json:"sequence"`
	Phoneme     string `json:"phoneme"`
	CategoryID  string `json:"categoryId"`
	StartSample int64  `json:"startSample"`
	EndSample   int64  `json:"endSample"`
	StartMs     int64  `json:"startMs"`
	EndMs       int64  `json:"endMs"`
}

type V3Transition struct {
	AtFrame          int    `json:"atFrame"`
	FromCategoryID   string `json:"fromCategoryId"`
	ToCategoryID     string `json:"toCategoryId"`
	BridgeCategoryID string `json:"bridgeCategoryId"`
	OffsetFrames     int    `json:"offsetFrames"`
	Reason           string `json:"reason"`
}

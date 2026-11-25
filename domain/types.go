package domain

const (
	AlgoVersion      = "1.0.0"
	PadDisposition   = "padded"
	FullDisposition  = "full"
	TruncDisposition = "truncated"
)

type Manifest struct {
	Channels          int          `json:"channels"`
	SampleRate        int64        `json:"sampleRate"`
	TotalSamples      int          `json:"totalSamples"`
	WindowSamples     int          `json:"windowSamples"`
	QuantUnit         int64        `json:"quantUnit"`
	QuantMode         string       `json:"quantMode"`
	TimingWindowMs    int64        `json:"timingWindowMs"`
	MaxSamplesPerPack int          `json:"maxSamplesPerPack"`
	TailRule          string       `json:"tailRule"`
	FlagMask          int64        `json:"flagMask"`
	GainDurationK     int64        `json:"gainDurationK"`
	TemplateCount     int          `json:"templateCount"`
	Templates         [][]Template `json:"templates"`
	BranchVersion     string       `json:"branchVersion"`
}

type Template struct {
	AttackPermille  int64 `json:"attackPermille"`
	ReleasePermille int64 `json:"releasePermille"`
}

type RawHeader struct {
	Channels     int   `json:"channels"`
	TotalSamples int   `json:"totalSamples"`
	SampleRate   int64 `json:"sampleRate"`
}

type RawEvent struct {
	ID  int64  `json:"id"`
	Ch  *int   `json:"ch,omitempty"`
	Ts  *int64 `json:"ts"`
	Ld  *int64 `json:"ld"`
	Tg  *int64 `json:"tg"`
	Pr  *int   `json:"pr"`
	Ni  *int   `json:"ni"`
	Cd  *int   `json:"cd"`
	Fl  *int64 `json:"fl"`
	Lst []int  `json:"lst"`
}

type BranchFile struct {
	Branch        string     `json:"branch"`
	BranchVersion string     `json:"branchVersion"`
	Header        RawHeader  `json:"header"`
	Events        []RawEvent `json:"events"`
}

type Event struct {
	ID             int64
	SourceBranch   string
	SourceOrdinal  int
	SourcePath     string
	Channel        int
	Start          int64
	Duration       int64
	Gain           int64
	Priority       int
	NoInterrupt    bool
	Conditional    bool
	Flag           int64
	FlagTrue       bool
	TypeSequence   []int
	RouteSequence  []int
	ArrivalOrder   int
	StartRaw       int64
	DurationRaw    int64
	GainRaw        int64
	StartBasis     string
	DurationBasis  string
	EffectiveEnd   int64
	ConflictAction string
}

type SampleSet struct {
	Manifest Manifest
	Branches map[string]BranchFile
	Events   []Event
}

type Packet struct {
	Window          int     `json:"window"`
	Channel         int     `json:"channel"`
	Seq             int     `json:"seq"`
	Start           int     `json:"start"`
	Samples         int     `json:"samples"`
	CapacitySamples int     `json:"capacitySamples"`
	Disposition     string  `json:"disposition"`
	Mark            int64   `json:"mark"`
	PrevOffset      int64   `json:"prevOffset"`
	Frames          []int64 `json:"frames"`
	PaddedFrames    []int64 `json:"paddedFrames"`
	EventIDs        []int64 `json:"eventIds"`
	TemplateHash    string  `json:"templateHash"`
	QuantUnit       int64   `json:"quantUnit"`
	ContentHash     string  `json:"sha256"`
	ConsumedOffset  int64   `json:"consumedOffset"`
	BranchVersion   string  `json:"branchVersion"`
	AlgoVersion     string  `json:"algoVersion"`
}

type ChannelResult struct {
	Channel int
	Frames  []int64
	Packets []Packet
	Events  []Event
}

type Report struct {
	Frames  map[int][]int64
	Packets []Packet
	Events  []Event
}

type Invalidation struct {
	Window  int    `json:"window"`
	Channel int    `json:"channel"`
	Seq     int    `json:"seq"`
	Reason  string `json:"reason"`
}

type QueueRecord struct {
	Reason    string `json:"reason"`
	Channel   int    `json:"channel"`
	WaitNanos int64  `json:"waitNanos"`
}

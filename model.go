package acoustic

const (
	FixedScale              int64 = 1000
	SchemaVersionCurrent          = 1
	AlgorithmVersionCurrent       = "reflection-fixed-v1"
	AbsorptionFixedScale    int64 = 1_000_000
	MaxAbsFixedCoordinate   int64 = 100_000_000
)

type Fixed = int64

type Point struct {
	X Fixed `json:"x"`
	Y Fixed `json:"y"`
}

type Rect struct {
	MinX Fixed `json:"minX"`
	MinY Fixed `json:"minY"`
	MaxX Fixed `json:"maxX"`
	MaxY Fixed `json:"maxY"`
}

type Room struct {
	ID   int  `json:"id"`
	Rect Rect `json:"rect"`
}

type Wall struct {
	ID    int   `json:"id"`
	RoomA int   `json:"roomA"`
	RoomB int   `json:"roomB"`
	A     Point `json:"a"`
	B     Point `json:"b"`
}

type Opening struct {
	ID     int   `json:"id"`
	WallID int   `json:"wallId"`
	Start  Fixed `json:"start"`
	Width  Fixed `json:"width"`
}

type Reflector struct {
	ID         int     `json:"id"`
	RoomID     int     `json:"roomId"`
	A          Point   `json:"a"`
	B          Point   `json:"b"`
	NX         Fixed   `json:"nx"`
	NY         Fixed   `json:"ny"`
	Absorption float64 `json:"absorption"`
}

type Source struct {
	ID      int     `json:"id"`
	RoomID  int     `json:"roomId"`
	Pos     Point   `json:"pos"`
	LevelDB float64 `json:"levelDb"`
}

type Listener struct {
	ID     int   `json:"id"`
	RoomID int   `json:"roomId"`
	Pos    Point `json:"pos"`
}

type Tolerance struct {
	CoordinateTicks Fixed   `json:"coordinateTicks"`
	PathTicks       Fixed   `json:"pathTicks"`
	IncidenceCos    int64   `json:"incidenceCos"`
	SPLDB           float64 `json:"splDb"`
}

type TruthPath struct {
	SourceID        int     `json:"sourceId"`
	ReflectorID     int     `json:"reflectorId"`
	ReflectionPoint Point   `json:"reflectionPoint"`
	PathLengthTicks Fixed   `json:"pathLengthTicks"`
	IncidenceCos    int64   `json:"incidenceCos"`
	SPLDB           float64 `json:"splDb"`
}

type TruthSource struct {
	SourceID         int         `json:"sourceId"`
	ChainRoomIDs     []int       `json:"chainRoomIds,omitempty"`
	ChainOpeningIDs  []int       `json:"chainOpeningIds,omitempty"`
	ChainLengthTicks Fixed       `json:"chainLengthTicks"`
	ValidPathCount   int         `json:"validPathCount"`
	Paths            []TruthPath `json:"paths"`
}

type ListenerTruth struct {
	ListenerID     int           `json:"listenerId"`
	RoomID         int           `json:"roomId"`
	ValidPathCount int           `json:"validPathCount"`
	Sources        []TruthSource `json:"sources"`
}

type Project struct {
	SchemaVersion    int             `json:"schemaVersion"`
	ParamsVersion    int64           `json:"paramsVersion"`
	AlgorithmVersion string          `json:"algorithmVersion"`
	Rooms            []Room          `json:"rooms"`
	Walls            []Wall          `json:"walls"`
	Openings         []Opening       `json:"openings"`
	Sources          []Source        `json:"sources"`
	Reflectors       []Reflector     `json:"reflectors"`
	Listeners        []Listener      `json:"listeners"`
	Tolerance        Tolerance       `json:"tolerance"`
	Truth            []ListenerTruth `json:"truth"`
}

func (p *Project) roomByID(id int) (*Room, bool) {
	for i := range p.Rooms {
		if p.Rooms[i].ID == id {
			return &p.Rooms[i], true
		}
	}
	return nil, false
}

func (p *Project) wallByID(id int) (*Wall, bool) {
	for i := range p.Walls {
		if p.Walls[i].ID == id {
			return &p.Walls[i], true
		}
	}
	return nil, false
}

func (p *Project) openingByID(id int) (*Opening, bool) {
	for i := range p.Openings {
		if p.Openings[i].ID == id {
			return &p.Openings[i], true
		}
	}
	return nil, false
}

func (p *Project) sourceByID(id int) (*Source, bool) {
	for i := range p.Sources {
		if p.Sources[i].ID == id {
			return &p.Sources[i], true
		}
	}
	return nil, false
}

func (p *Project) listenerByID(id int) (*Listener, bool) {
	for i := range p.Listeners {
		if p.Listeners[i].ID == id {
			return &p.Listeners[i], true
		}
	}
	return nil, false
}

func (p *Project) reflectorByID(id int) (*Reflector, bool) {
	for i := range p.Reflectors {
		if p.Reflectors[i].ID == id {
			return &p.Reflectors[i], true
		}
	}
	return nil, false
}

func (r Rect) Width() Fixed {
	return r.MaxX - r.MinX
}

func (r Rect) Height() Fixed {
	return r.MaxY - r.MinY
}

func (r Rect) ContainsInterior(p Point) bool {
	return p.X > r.MinX && p.X < r.MaxX && p.Y > r.MinY && p.Y < r.MaxY
}

func (r Rect) ContainsClosed(p Point) bool {
	return p.X >= r.MinX && p.X <= r.MaxX && p.Y >= r.MinY && p.Y <= r.MaxY
}

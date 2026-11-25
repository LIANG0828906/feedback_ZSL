package acoustic

import (
	"math"
	"math/big"
)

type ErrorPropagation struct {
	ChainSegments               int     `json:"chainSegments"`
	ChainResidualTicks          float64 `json:"chainResidualTicks"`
	ChainLengthUpperBoundDB     float64 `json:"chainLengthUpperBoundDb"`
	MirrorResidualTicks         float64 `json:"mirrorResidualTicks"`
	MirrorRoundTripUpperBoundDB float64 `json:"mirrorRoundTripUpperBoundDb"`
	DistanceResidualTicks       float64 `json:"distanceResidualTicks"`
	DistanceRoundUpperBoundDB   float64 `json:"distanceRoundUpperBoundDb"`
	AbsorptionQuantizationDB    float64 `json:"absorptionQuantizationDb"`
	CumulativeUpperBoundDB      float64 `json:"cumulativeUpperBoundDb"`
}

type ReflectionPath struct {
	SourceID         int              `json:"sourceId"`
	ReflectorID      int              `json:"reflectorId"`
	ReflectionPoint  Point            `json:"reflectionPoint"`
	PathLengthTicks  Fixed            `json:"pathLengthTicks"`
	IncidenceCos     int64            `json:"incidenceCos"`
	SPLDB            float64          `json:"splDb"`
	Absorption       float64          `json:"absorption"`
	ErrorPropagation ErrorPropagation `json:"errorPropagation"`
}

type ReflectionRejection struct {
	SourceID    int    `json:"sourceId"`
	ReflectorID int    `json:"reflectorId"`
	Reason      string `json:"reason"`
}

type SourceReflectionResult struct {
	SourceID   int                   `json:"sourceId"`
	Chain      ChainSummary          `json:"chain"`
	Paths      []ReflectionPath      `json:"paths"`
	Rejections []ReflectionRejection `json:"rejections,omitempty"`
}

type ListenerSolution struct {
	ListenerID     int                      `json:"listenerId"`
	RoomID         int                      `json:"roomId"`
	ValidPathCount int                      `json:"validPathCount"`
	Sources        []SourceReflectionResult `json:"sources"`
	Paths          []ReflectionPath         `json:"paths"`
}

func SolveListener(project *Project, listenerID int) (ListenerSolution, error) {
	if project == nil {
		return ListenerSolution{}, &Error{Code: CodeInternal, Op: "reflection", Message: "project is nil", ListenerIDs: []int{listenerID}}
	}
	listener, ok := project.listenerByID(listenerID)
	if !ok {
		return ListenerSolution{}, &Error{Code: CodeSchema, Op: "reflection", ListenerIDs: []int{listenerID}, Message: "listener does not exist"}
	}
	reflectors := reflectorsForRoom(project, listener.RoomID)
	sources := append([]Source(nil), project.Sources...)
	sortSourcesByID(sources)
	solution := ListenerSolution{ListenerID: listener.ID, RoomID: listener.RoomID}
	for _, source := range sources {
		chain, err := FindChain(project, source.ID, listener.ID)
		if err != nil {
			return ListenerSolution{}, noChainError(source.ID, listener.ID, err)
		}
		sourceResult := SourceReflectionResult{SourceID: source.ID, Chain: chain}
		for _, reflector := range reflectors {
			path, rejection, err := solveReflection(project, source, *listener, reflector, chain)
			if err != nil {
				return ListenerSolution{}, err
			}
			if rejection != "" {
				sourceResult.Rejections = append(sourceResult.Rejections, ReflectionRejection{
					SourceID:    source.ID,
					ReflectorID: reflector.ID,
					Reason:      rejection,
				})
				continue
			}
			sourceResult.Paths = append(sourceResult.Paths, path)
			solution.Paths = append(solution.Paths, path)
			solution.ValidPathCount++
		}
		solution.Sources = append(solution.Sources, sourceResult)
	}
	sortReflectionPaths(solution.Paths)
	for i := range solution.Sources {
		sortReflectionPaths(solution.Sources[i].Paths)
	}
	return solution, nil
}

func SolveSourceListener(project *Project, sourceID, listenerID int) (SourceReflectionResult, error) {
	if project == nil {
		return SourceReflectionResult{}, &Error{Code: CodeInternal, Op: "reflection", Message: "project is nil", SourceIDs: []int{sourceID}, ListenerIDs: []int{listenerID}}
	}
	source, ok := project.sourceByID(sourceID)
	if !ok {
		return SourceReflectionResult{}, &Error{Code: CodeSchema, Op: "reflection", SourceIDs: []int{sourceID}, ListenerIDs: []int{listenerID}, Message: "source does not exist"}
	}
	listener, ok := project.listenerByID(listenerID)
	if !ok {
		return SourceReflectionResult{}, &Error{Code: CodeSchema, Op: "reflection", SourceIDs: []int{sourceID}, ListenerIDs: []int{listenerID}, Message: "listener does not exist"}
	}
	chain, err := FindChain(project, sourceID, listenerID)
	if err != nil {
		return SourceReflectionResult{}, noChainError(sourceID, listenerID, err)
	}
	result := SourceReflectionResult{SourceID: sourceID, Chain: chain}
	for _, reflector := range reflectorsForRoom(project, listener.RoomID) {
		path, rejection, err := solveReflection(project, *source, *listener, reflector, chain)
		if err != nil {
			return SourceReflectionResult{}, err
		}
		if rejection != "" {
			result.Rejections = append(result.Rejections, ReflectionRejection{
				SourceID:    sourceID,
				ReflectorID: reflector.ID,
				Reason:      rejection,
			})
			continue
		}
		result.Paths = append(result.Paths, path)
	}
	sortReflectionPaths(result.Paths)
	return result, nil
}

func solveReflection(project *Project, source Source, listener Listener, reflector Reflector, chain ChainSummary) (ReflectionPath, string, error) {
	if reflector.RoomID != listener.RoomID {
		return ReflectionPath{}, "reflector_not_in_listener_room", nil
	}
	line := pointSub(reflector.B, reflector.A)
	if line == (Point{}) {
		return ReflectionPath{}, "degenerate_reflector", nil
	}
	d := reflector.NX*(source.Pos.X-reflector.A.X) + reflector.NY*(source.Pos.Y-reflector.A.Y)
	e := reflector.NX*(listener.Pos.X-reflector.A.X) + reflector.NY*(listener.Pos.Y-reflector.A.Y)
	if d == 0 || e == 0 {
		return ReflectionPath{}, "source_or_listener_on_reflector", nil
	}
	if (d > 0) != (e > 0) {
		return ReflectionPath{}, "source_and_listener_on_opposite_sides", nil
	}
	point, residual, err := mirrorIntersection(source.Pos, listener.Pos, reflector, d, e)
	if err != nil {
		return ReflectionPath{}, "", err
	}
	if !pointOnSegmentTolerance(point, reflector.A, reflector.B, 2) {
		return ReflectionPath{}, "reflection_point_outside_reflector", nil
	}
	room, ok := project.roomByID(listener.RoomID)
	if !ok {
		return ReflectionPath{}, "", &Error{Code: CodeSchema, Op: "reflection", RoomIDs: []int{listener.RoomID}, ListenerIDs: []int{listener.ID}, Message: "listener room does not exist"}
	}
	if !room.Rect.ContainsClosed(point) {
		return ReflectionPath{}, "reflection_point_outside_listener_room", nil
	}
	if !segmentClear(project, source.Pos, point) {
		return ReflectionPath{}, "source_to_reflection_blocked", nil
	}
	if !segmentClear(project, point, listener.Pos) {
		return ReflectionPath{}, "reflection_to_listener_blocked", nil
	}
	d1, r1 := distanceTicks(source.Pos, point)
	d2, r2 := distanceTicks(point, listener.Pos)
	if d1 <= 0 || d2 <= 0 {
		return ReflectionPath{}, "zero_length_reflection_leg", nil
	}
	pathLength := d1 + d2
	cosine := incidenceCosine(source.Pos, point, reflector)
	absorptionFixed := math.Round(reflector.Absorption*float64(AbsorptionFixedScale)) / float64(AbsorptionFixedScale)
	pathWorld := float64(pathLength) / float64(FixedScale)
	spl := source.LevelDB - 20*math.Log10(pathWorld) - 8.685889638*absorptionFixed*pathWorld
	report := propagationReport(chain, residual, r1+r2, pathWorld, absorptionFixed, reflector.NX, reflector.NY)
	return ReflectionPath{
		SourceID:         source.ID,
		ReflectorID:      reflector.ID,
		ReflectionPoint:  point,
		PathLengthTicks:  Fixed(pathLength),
		IncidenceCos:     cosine,
		SPLDB:            spl,
		Absorption:       absorptionFixed,
		ErrorPropagation: report,
	}, "", nil
}

func mirrorIntersection(source, listener Point, reflector Reflector, d, e int64) (Point, float64, error) {
	nx := big.NewInt(reflector.NX)
	ny := big.NewInt(reflector.NY)
	q := new(big.Int).Add(new(big.Int).Mul(nx, nx), new(big.Int).Mul(ny, ny))
	db := big.NewInt(d)
	eb := big.NewInt(e)
	sx := big.NewInt(source.X)
	sy := big.NewInt(source.Y)
	tx := big.NewInt(listener.X)
	ty := big.NewInt(listener.Y)

	den := new(big.Int).Mul(q, new(big.Int).Add(db, eb))
	if den.Sign() == 0 {
		return Point{}, 0, &Error{Code: CodeInternal, Op: "reflection", Message: "zero mirror intersection denominator"}
	}
	termX := new(big.Int).Add(new(big.Int).Mul(eb, sx), new(big.Int).Mul(db, tx))
	termY := new(big.Int).Add(new(big.Int).Mul(eb, sy), new(big.Int).Mul(db, ty))
	numX := new(big.Int).Mul(q, termX)
	numY := new(big.Int).Mul(q, termY)
	mirrorCorrection := new(big.Int).Mul(big.NewInt(2), new(big.Int).Mul(db, eb))
	numX.Sub(numX, new(big.Int).Mul(mirrorCorrection, nx))
	numY.Sub(numY, new(big.Int).Mul(mirrorCorrection, ny))

	x, xExact, err := roundRational(numX, den)
	if err != nil {
		return Point{}, 0, err
	}
	y, yExact, err := roundRational(numY, den)
	if err != nil {
		return Point{}, 0, err
	}
	dx := xExact - float64(x)
	dy := yExact - float64(y)
	return Point{X: x, Y: y}, math.Hypot(dx, dy), nil
}

func roundRational(num, den *big.Int) (int64, float64, error) {
	if den.Sign() == 0 {
		return 0, 0, &Error{Code: CodeInternal, Op: "reflection", Message: "rational denominator is zero"}
	}
	if den.Sign() < 0 {
		num = new(big.Int).Neg(num)
		den = new(big.Int).Neg(den)
	}
	quo, rem := new(big.Int).QuoRem(num, den, new(big.Int))
	absRem := new(big.Int).Abs(rem)
	if new(big.Int).Lsh(absRem, 1).Cmp(den) >= 0 {
		if num.Sign() >= 0 {
			quo.Add(quo, big.NewInt(1))
		} else {
			quo.Sub(quo, big.NewInt(1))
		}
	}
	if !quo.IsInt64() {
		return 0, 0, &Error{Code: CodeInternal, Op: "reflection", Message: "reflection coordinate exceeds fixed-point range"}
	}
	rounded := quo.Int64()
	rat := new(big.Rat).SetFrac(num, den)
	exact, _ := rat.Float64()
	return rounded, exact, nil
}

func incidenceCosine(source, reflection Point, reflector Reflector) int64 {
	vx := float64(source.X - reflection.X)
	vy := float64(source.Y - reflection.Y)
	length := math.Hypot(vx, vy)
	normalLength := math.Hypot(float64(reflector.NX), float64(reflector.NY))
	if length == 0 || normalLength == 0 {
		return 0
	}
	dot := math.Abs(vx*float64(reflector.NX) + vy*float64(reflector.NY))
	cosine := dot / (length * normalLength)
	if cosine < 0 {
		cosine = 0
	}
	if cosine > 1 {
		cosine = 1
	}
	return int64(math.Round(cosine * float64(FixedScale)))
}

func propagationReport(chain ChainSummary, mirrorResidual, distanceResidual float64, pathWorld, absorption float64, nx, ny Fixed) ErrorPropagation {
	pathDBPerWorld := 20/(math.Ln10*pathWorld) + 8.685889638*absorption
	pathDBPerTick := pathDBPerWorld / float64(FixedScale)
	chainBoundTicks := float64(len(chain.Segments)) * 0.5
	mirrorBoundTicks := 2 * math.Sqrt(2) * 0.5
	distanceBoundTicks := 1.0
	alphaStep := 1 / float64(AbsorptionFixedScale)
	alphaBoundDB := 8.685889638 * pathWorld * alphaStep * 0.5
	chainDB := chainBoundTicks * pathDBPerTick
	mirrorDB := mirrorBoundTicks * pathDBPerTick
	distanceDB := distanceBoundTicks * pathDBPerTick
	return ErrorPropagation{
		ChainSegments:               len(chain.Segments),
		ChainResidualTicks:          math.Abs(chain.QuantizationResidual),
		ChainLengthUpperBoundDB:     chainDB,
		MirrorResidualTicks:         mirrorResidual,
		MirrorRoundTripUpperBoundDB: mirrorDB,
		DistanceResidualTicks:       math.Abs(distanceResidual),
		DistanceRoundUpperBoundDB:   distanceDB,
		AbsorptionQuantizationDB:    alphaBoundDB,
		CumulativeUpperBoundDB:      chainDB + mirrorDB + distanceDB + alphaBoundDB,
	}
}

func reflectorsForRoom(project *Project, roomID int) []Reflector {
	var reflectors []Reflector
	for _, reflector := range project.Reflectors {
		if reflector.RoomID == roomID {
			reflectors = append(reflectors, reflector)
		}
	}
	sortReflectorsByID(reflectors)
	return reflectors
}

func sortReflectorsByID(reflectors []Reflector) {
	for i := 1; i < len(reflectors); i++ {
		for j := i; j > 0 && reflectors[j].ID < reflectors[j-1].ID; j-- {
			reflectors[j], reflectors[j-1] = reflectors[j-1], reflectors[j]
		}
	}
}

func sortSourcesByID(sources []Source) {
	for i := 1; i < len(sources); i++ {
		for j := i; j > 0 && sources[j].ID < sources[j-1].ID; j-- {
			sources[j], sources[j-1] = sources[j-1], sources[j]
		}
	}
}

func sortReflectionPaths(paths []ReflectionPath) {
	for i := 1; i < len(paths); i++ {
		for j := i; j > 0; j-- {
			left, right := paths[j-1], paths[j]
			if left.SourceID < right.SourceID || (left.SourceID == right.SourceID && left.ReflectorID <= right.ReflectorID) {
				break
			}
			paths[j], paths[j-1] = paths[j-1], paths[j]
		}
	}
}

func noChainError(sourceID, listenerID int, cause error) *Error {
	return &Error{
		Code:        CodeNoChain,
		Op:          "chain",
		Message:     "no connected room chain",
		SourceIDs:   []int{sourceID},
		ListenerIDs: []int{listenerID},
		Cause:       cause.Error(),
	}
}

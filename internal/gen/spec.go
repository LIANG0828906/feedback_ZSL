package gen

import "coldkernel/internal/model"

// Spec is the canonical, version-independent description of one generated
// project. It is mapped to all three schema snapshots by the same seed.
type Spec struct {
	Seed             uint64
	TickMs           int64
	HorizonTicks     int64
	ConversionWidth  int
	FixedTolerance   int64
	FunctionCount    int
	RequestsPerFunc  int
	Tenants          []string
	Functions        []FuncSpec
	Requests         []ReqSpec
}

// FuncSpec declares one function in canonical tick/absolute terms.
type FuncSpec struct {
	ID               string
	ConcurrencyLimit int64
	Reserved         int64
	WarmPoolSize     int64
	ColdMinTicks     int64
	ColdMaxTicks     int64
	ColdTimeoutTicks int64
	BudgetPermyriad  int64 // ‱
	QueueLimit       int64
	WaitLimitTicks   int64
}

// ReqSpec declares one request in canonical terms.
type ReqSpec struct {
	ID         string
	FunctionID string
	ArriveTick int64
	Slots      int64
	Priority   model.Priority
	Tenant     string
	HoldTicks  int64
}

// Generate builds a deterministic Spec from a seed. The same seed always
// yields the same functions, warm pools and request arrivals.
func Generate(seed uint64) *Spec {
	rng := NewRNG(seed)
	spec := &Spec{
		Seed:            seed,
		TickMs:          100,
		HorizonTicks:    60,
		ConversionWidth: 64,
		FixedTolerance:  1,
		FunctionCount:   4,
		RequestsPerFunc: 18,
		Tenants:         []string{"tenant-a", "tenant-b", "tenant-c"},
	}

	for fi := 0; fi < spec.FunctionCount; fi++ {
		id := funcID(fi)
		limit := int64(4 + fi*2) // 4,6,8,10
		reserved := rng.Range(1, limit/2)
		pool := rng.Range(0, reserved)
		fs := FuncSpec{
			ID:               id,
			ConcurrencyLimit: limit,
			Reserved:         reserved,
			WarmPoolSize:     pool,
			ColdMinTicks:     int64(2 + rng.Intn(2)),
			ColdMaxTicks:     int64(5 + rng.Intn(4)),
			ColdTimeoutTicks: 12,
			// Multiple of 10 keeps the v1 ‰ -> ‱ scale change exact so all
			// three versions resolve the same absolute cold budget.
			BudgetPermyriad:  int64(4000 + rng.Intn(300)*10), // 40%-69.9%
			QueueLimit:       int64(3 + rng.Intn(3)),
			WaitLimitTicks:   8,
		}
		spec.Functions = append(spec.Functions, fs)
	}

	seq := 0
	for fi := 0; fi < spec.FunctionCount; fi++ {
		fs := spec.Functions[fi]
		// Independent per-function stream keeps parallel evaluation
		// deterministic regardless of scheduling order.
		frng := NewRNG(seed ^ HashString(fs.ID))
		for ri := 0; ri < spec.RequestsPerFunc; ri++ {
			arrive := frng.Intn(spec.HorizonTicks - 4)
			slots := int64(1 + frng.Intn(2))
			// Bias traffic so reserved and shared classes both appear and
			// boundary slots (last slot fill) are exercised.
			prio := model.PriorityShared
			if frng.Intn(2) == 0 {
				prio = model.PriorityReserved
			}
			spec.Requests = append(spec.Requests, ReqSpec{
				ID:         reqID(seq),
				FunctionID: fs.ID,
				ArriveTick: arrive,
				Slots:      slots,
				Priority:   prio,
				Tenant:     frng.Pick(spec.Tenants),
				HoldTicks:  int64(2 + frng.Intn(6)),
			})
			seq++
		}
	}
	// Canonical order: arrival tick, then function, then request id.
	sortRequests(spec.Requests)
	return spec
}

func funcID(i int) string {
	return "fn-" + string(rune('a'+i))
}

func reqID(i int) string {
	const digits = "0123456789"
	if i < 100 {
		return "req-" + string(digits[i/10]) + string(digits[i%10])
}
	return "req-" + itoa(i)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [12]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	return string(b[n:])
}

// HashString folds an id into a uint64 for stream seeding (FNV-1a).
func HashString(s string) uint64 {
	const offset = 14695981039346656037
	const prime = 1099511628211
	h := uint64(offset)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime
	}
	return h
}

func sortRequests(rs []ReqSpec) {
	// Stable insertion sort; inputs are small and determinism matters more
	// than raw speed.
	for i := 1; i < len(rs); i++ {
		for j := i; j > 0 && less(rs[j], rs[j-1]); j-- {
			rs[j], rs[j-1] = rs[j-1], rs[j]
		}
	}
}

func less(a, b ReqSpec) bool {
	if a.ArriveTick != b.ArriveTick {
		return a.ArriveTick < b.ArriveTick
	}
	if a.FunctionID != b.FunctionID {
		return a.FunctionID < b.FunctionID
	}
	return a.ID < b.ID
}

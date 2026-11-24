// Package prng 提供纯确定性的伪随机源。
// 随机判定只能由对局种子与回合号派生子随机源，不依赖全局状态或 goroutine 标识。
package prng

// Source 是 splitmix64 实现，零值不可直接使用，请用 New 构造。
type Source struct {
	state uint64
	draws int
}

// New 用种子构造一个独立子随机源。
func New(seed uint64) *Source {
	return &Source{state: seed}
}

// Uint64 返回下一个确定的 64 位随机数。
func (s *Source) Uint64() uint64 {
	s.state += 0x9e3779b97f4a7c15
	s.draws++
	z := s.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// Intn 返回 [0,n) 内确定随机整数，n 必须为正。
func (s *Source) Intn(n int) int {
	if n <= 0 {
		panic("prng: Intn bound must be positive")
	}
	return int(s.Uint64() % uint64(n))
}

// Chance 以 basis/1000 的概率返回 true（定点概率，basis 为千分比）。
func (s *Source) Chance(basisPerMille int) bool {
	return int(s.Uint64()%1000) < basisPerMille
}

// Draws 返回该随机源已抽取次数，用于子随机源进度检查点。
func (s *Source) Draws() int {
	return s.draws
}

// TurnSeed 由对局种子与回合号派生该回合的子随机源种子。
// 与回合完成顺序、线程标识完全无关，仅取决于两个确定性输入。
func TurnSeed(matchSeed int64, turnNo int) uint64 {
	x := uint64(matchSeed) ^ (uint64(turnNo) * 0x9e3779b97f4a7c15)
	x ^= 0xdeadbeefcafebabe
	// splitmix64 一次混合，打散相邻回合种子。
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

// Mix64 用于哈希链混合等场景。
func Mix64(x uint64) uint64 {
	x ^= x >> 33
	x *= 0xff51afd7ed558ccd
	x ^= x >> 33
	x *= 0xc4ceb9fe1a85ec53
	x ^= x >> 33
	return x
}

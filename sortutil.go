package acousticfield

import "sort"

func sortStrings(v []string) {
	sort.Strings(v)
}

func sortBandEnergy(v []BandEnergy) {
	sort.Slice(v, func(i, j int) bool { return v[i].BandID < v[j].BandID })
}

func sortAbsorption(v []Absorption) {
	sort.Slice(v, func(i, j int) bool { return v[i].BandID < v[j].BandID })
}

func sortReflectors(v []Reflector) {
	sort.Slice(v, func(i, j int) bool { return v[i].ID < v[j].ID })
}

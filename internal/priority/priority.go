package priority

import (
	"net/http"
	"strconv"
	"strings"

	"feedback_zsl/internal/config"
)

type Class struct {
	Name string
	Rank int
}

type Mapping struct {
	Priority    Class
	Source      string
	H2Weight    int
	H3Urgency   int
	HasH2Weight bool
	HasH3       bool
}

type Mapper struct {
	byName      map[string]Class
	header      map[string]Class
	h3          map[int]Class
	h2Ranges    []h2Range
	defaultPrio Class
}

type h2Range struct {
	min, max int
	class    Class
}

func New(cfg *config.Config) *Mapper {
	m := &Mapper{
		byName: make(map[string]Class),
		header: make(map[string]Class),
		h3:     make(map[int]Class),
	}
	for _, p := range cfg.Priorities {
		c := Class{Name: p.Name, Rank: p.Rank}
		m.byName[p.Name] = c
		m.h2Ranges = append(m.h2Ranges, h2Range{min: p.H2WeightMin, max: p.H2WeightMax, class: c})
		for _, u := range p.H3Urgency {
			m.h3[u] = c
		}
		for _, v := range p.HeaderValues {
			m.header[strings.ToLower(v)] = c
		}
	}
	m.defaultPrio = m.byName[cfg.DefaultPrio]
	return m
}

func (m *Mapper) Default() Class {
	return m.defaultPrio
}

func (m *Mapper) ByName(name string) (Class, bool) {
	c, ok := m.byName[strings.ToLower(strings.TrimSpace(name))]
	return c, ok
}

func (m *Mapper) FromHeaders(h http.Header) (Mapping, bool) {
	if v := strings.TrimSpace(h.Get("X-Gateway-Priority")); v != "" {
		key := strings.ToLower(v)
		if c, ok := m.byName[key]; ok {
			return Mapping{Priority: c, Source: "header_class"}, true
		}
		if c, ok := m.header[key]; ok {
			return Mapping{Priority: c, Source: "header_class_alias"}, true
		}
	}
	raw := strings.TrimSpace(h.Get("Priority"))
	if raw == "" {
		return Mapping{}, false
	}
	if urgency, ok := parseUrgency(raw); ok {
		if c, found := m.h3[urgency]; found {
			return Mapping{
				Priority:  c,
				Source:    "rfc9218_priority_header",
				H3Urgency: urgency,
				HasH3:     true,
			}, true
		}
	}
	return Mapping{}, false
}

func (m *Mapper) FromH2Weight(wireWeight uint8, header http.Header) Mapping {
	if v := strings.TrimSpace(header.Get("X-Gateway-Priority")); v != "" {
		key := strings.ToLower(v)
		if c, ok := m.byName[key]; ok {
			return Mapping{Priority: c, Source: "header_class", H2Weight: int(wireWeight) + 1, HasH2Weight: true}
		}
		if c, ok := m.header[key]; ok {
			return Mapping{Priority: c, Source: "header_class_alias", H2Weight: int(wireWeight) + 1, HasH2Weight: true}
		}
	}
	weight := int(wireWeight) + 1
	for _, r := range m.h2Ranges {
		if weight >= r.min && weight <= r.max {
			return Mapping{
				Priority:    r.class,
				Source:      "h2_priority_frame",
				H2Weight:    weight,
				HasH2Weight: true,
			}
		}
	}
	return Mapping{
		Priority:    m.defaultPrio,
		Source:      "default",
		H2Weight:    weight,
		HasH2Weight: true,
	}
}

// ParsePriorityHeader is exported for diagnostics and tests.
func ParsePriorityHeader(raw string) (urgency int, incremental bool, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false, false
	}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, value, _ := strings.Cut(part, "=")
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		switch key {
		case "u":
			n, err := strconv.Atoi(value)
			if err == nil && n >= 0 && n <= 7 {
				urgency = n
				ok = true
			}
		case "i":
			incremental = strings.EqualFold(value, "?1") || strings.EqualFold(value, "true") || value == "1"
		}
	}
	return urgency, incremental, ok
}

func parseUrgency(raw string) (int, bool) {
	u, _, ok := ParsePriorityHeader(raw)
	return u, ok
}

package ocrflow

import (
	"fmt"
	"sort"
	"unicode/utf8"
)

type Template struct {
	Rune    rune
	Pattern [7]string
	Version string
}

type TemplateLibrary struct {
	Name      string
	Version   string
	Hash      string
	Templates []Template
	byRune    map[rune]Template
}

func NewTemplateLibrary(name, version string, glyphs map[rune][7]string, extra ...func(map[rune][7]string)) *TemplateLibrary {
	source := make(map[rune][7]string, len(glyphs)+4)
	for r, p := range glyphs {
		source[r] = p
	}
	for _, fn := range extra {
		fn(source)
	}
	runes := make([]rune, 0, len(source))
	for r := range source {
		if r != ' ' {
			runes = append(runes, r)
		}
	}
	sort.Slice(runes, func(i, j int) bool { return runes[i] < runes[j] })
	lib := &TemplateLibrary{Name: name, Version: version, byRune: map[rune]Template{}}
	hashInput := name + "|" + version + "|"
	for _, r := range runes {
		p := source[r]
		t := Template{Rune: r, Pattern: p, Version: version}
		lib.Templates = append(lib.Templates, t)
		lib.byRune[r] = t
		hashInput += string(r) + ":" + fmt.Sprint(p) + ";"
	}
	lib.Hash = HashString(hashInput)[:16]
	return lib
}

func DefaultTemplateLibrary() *TemplateLibrary {
	return NewTemplateLibrary(DefaultTemplateName, TemplateVersionV1, bitmapFont)
}

func V2TemplateLibrary() *TemplateLibrary {
	return NewTemplateLibrary(DefaultTemplateName, TemplateVersionV2, bitmapFont, func(m map[rune][7]string) {
		m['!'] = [7]string{"..#..", "..#..", "..#..", "..#..", "..#..", ".....", "..#.."}
		m['?'] = [7]string{".###.", "#...#", "....#", "..##.", "..#..", ".....", "..#.."}
	})
}

type CharHit struct {
	Index           int    `json:"index"`
	Char            string `json:"char"`
	ConfidenceBPS   int    `json:"confidence_bps"`
	MatchBPS        int    `json:"match_bps"`
	TemplateID      string `json:"template_id"`
	TemplateVersion string `json:"template_version"`
	Box             Rect   `json:"box"`
}

type LineRecognition struct {
	LineNo          int       `json:"line_no"`
	LineID          string    `json:"line_id"`
	Text            string    `json:"text"`
	Empty           bool      `json:"empty"`
	CharCount       int       `json:"char_count"`
	Chars           []CharHit `json:"chars"`
	ConfidenceBPS   int       `json:"confidence_bps"`
	Segmentation    string    `json:"segmentation"`
	TemplateLibrary string    `json:"template_library"`
	TemplateVersion string    `json:"template_version"`
	TemplateHash    string    `json:"template_hash"`
}

type segment struct {
	Rect        Rect
	ComponentID int
	Sequence    bool
}

func RecognizeLine(line DetectedLine, detection *DetectionResult, mask *Mask, lib *TemplateLibrary, cfg RecogConfig) (*LineRecognition, error) {
	if lib == nil {
		return nil, fmt.Errorf("template library is required")
	}
	byID := make(map[int]Component, len(detection.Components))
	for _, c := range detection.Components {
		byID[c.ID] = c
	}
	segments := make([]segment, 0, len(line.ComponentIDs))
	for _, id := range line.ComponentIDs {
		c, ok := byID[id]
		if !ok {
			continue
		}
		sw := maxInt(line.StrokeWidthPx, 1)
		if cfg.SequenceSplit && c.Rect.Width() > 7*sw+8 {
			segments = append(segments, sequenceSplit(c.Rect, sw)...)
		} else {
			segments = append(segments, segment{Rect: c.Rect, ComponentID: id})
		}
	}
	sort.Slice(segments, func(i, j int) bool { return segments[i].Rect.MinX < segments[j].Rect.MinX })

	result := &LineRecognition{
		LineNo:          line.LineNo,
		LineID:          line.LineID,
		Chars:           []CharHit{},
		Segmentation:    "connected_components+stroke_pitch",
		TemplateLibrary: lib.Name,
		TemplateVersion: lib.Version,
		TemplateHash:    lib.Hash,
	}
	if len(segments) == 0 {
		result.Empty = true
		result.Text = ""
		result.CharCount = 0
		result.ConfidenceBPS = 0
		result.Segmentation = "empty_explicit"
		return result, nil
	}

	sw := maxInt(line.StrokeWidthPx, 1)
	var text []rune
	prevCenter := -1 << 30
	charIndex := 0
	confSum := 0
	for _, seg := range segments {
		center := seg.Rect.MinX + seg.Rect.Width()/2
		if len(text) > 0 && center-prevCenter > 9*sw {
			text = append(text, ' ')
		}
		r, score, err := matchSegment(seg.Rect, mask, lib)
		if err != nil {
			return nil, err
		}
		hit := CharHit{
			Index:           charIndex,
			Char:            string(r),
			ConfidenceBPS:   score,
			MatchBPS:        score,
			TemplateID:      fmt.Sprintf("%s@%s#%s", string(r), lib.Version, lib.Hash),
			TemplateVersion: lib.Version,
			Box:             seg.Rect,
		}
		result.Chars = append(result.Chars, hit)
		text = append(text, r)
		confSum += score
		charIndex++
		prevCenter = center
	}
	result.Text = string(text)
	result.CharCount = utf8.RuneCountInString(result.Text)
	result.ConfidenceBPS = confSum / maxInt(len(result.Chars), 1)
	if result.Text == "" {
		result.Empty = true
		result.Segmentation = "empty_explicit"
	}
	return result, nil
}

func sequenceSplit(r Rect, sw int) []segment {
	out := make([]segment, 0, 4)
	advance := 6 * sw
	glyphW := 5 * sw
	if advance <= 0 {
		return out
	}
	for x := r.MinX; x < r.MaxX; x += advance {
		end := minInt(x+glyphW, r.MaxX)
		if end-x < 2 {
			break
		}
		out = append(out, segment{Rect: Rect{MinX: x, MinY: r.MinY, MaxX: end, MaxY: r.MaxY}, Sequence: true})
	}
	return out
}

func matchSegment(r Rect, mask *Mask, lib *TemplateLibrary) (rune, int, error) {
	if r.Empty() {
		return 0, 0, fmt.Errorf("empty character segment")
	}
	var coverage [7][5]int
	for row := 0; row < 7; row++ {
		y0 := r.MinY + row*r.Height()/7
		y1 := r.MinY + (row+1)*r.Height()/7
		for col := 0; col < 5; col++ {
			x0 := r.MinX + col*r.Width()/5
			x1 := r.MinX + (col+1)*r.Width()/5
			area, hits := 0, 0
			for y := y0; y < y1; y++ {
				for x := x0; x < x1; x++ {
					area++
					if mask.At(x, y) != 0 {
						hits++
					}
				}
			}
			if area == 0 {
				coverage[row][col] = 0
			} else {
				coverage[row][col] = hits * 10000 / area
			}
		}
	}

	bestRune := rune('?')
	bestScore := -1
	secondScore := -1
	for _, tpl := range lib.Templates {
		sum := 0
		for row := 0; row < 7; row++ {
			for col := 0; col < 5; col++ {
				cov := coverage[row][col]
				if tpl.Pattern[row][col] == '#' {
					sum += cov
				} else {
					sum += 10000 - cov
				}
			}
		}
		score := sum / 35
		if score > bestScore {
			secondScore = bestScore
			bestScore = score
			bestRune = tpl.Rune
		} else if score > secondScore {
			secondScore = score
		}
	}
	if bestScore < 0 {
		return 0, 0, fmt.Errorf("template library %s@%s has no templates", lib.Name, lib.Version)
	}
	margin := bestScore - maxInt(secondScore, 0)
	confidence := (bestScore*85 + minInt(margin*5, 10000)*15) / 100
	return bestRune, clampInt(confidence, 0, 10000), nil
}

type LineAccuracy struct {
	LineNo         int    `json:"line_no"`
	Expected       string `json:"expected"`
	Recognized     string `json:"recognized"`
	Distance       int    `json:"distance"`
	ReferenceRunes int    `json:"reference_runes"`
	CERBPS         int    `json:"cer_bps"`
	Exact          bool   `json:"exact"`
}

type AccuracyReport struct {
	ShotID         string         `json:"shot_id"`
	Lines          []LineAccuracy `json:"lines"`
	TotalDistance  int            `json:"total_distance"`
	TotalRunes     int            `json:"total_runes"`
	CERBPS         int            `json:"cer_bps"`
	DetectedLines  int            `json:"detected_lines"`
	TruthLines     int            `json:"truth_lines"`
	LineCountDelta int            `json:"line_count_delta"`
}

func CompareTruth(shot *Screenshot, recognitions []LineRecognition) AccuracyReport {
	report := AccuracyReport{ShotID: shot.ShotID, TruthLines: len(shot.Lines), DetectedLines: len(recognitions)}
	report.LineCountDelta = report.DetectedLines - report.TruthLines
	maxLines := maxInt(len(shot.Lines), len(recognitions))
	for i := 0; i < maxLines; i++ {
		truth := ""
		hyp := ""
		lineNo := i + 1
		if i < len(shot.Lines) {
			truth = shot.Lines[i].Text
			lineNo = shot.Lines[i].LineNo
		}
		if i < len(recognitions) {
			hyp = recognitions[i].Text
			lineNo = recognitions[i].LineNo
		}
		dist, runes := CharacterErrorRate(truth, hyp)
		cer := 0
		if runes > 0 {
			cer = dist * 10000 / runes
		}
		la := LineAccuracy{LineNo: lineNo, Expected: truth, Recognized: hyp, Distance: dist, ReferenceRunes: runes, CERBPS: cer, Exact: truth == hyp}
		report.Lines = append(report.Lines, la)
		report.TotalDistance += dist
		report.TotalRunes += runes
	}
	if report.TotalRunes > 0 {
		report.CERBPS = report.TotalDistance * 10000 / report.TotalRunes
	}
	return report
}

func CharacterErrorRate(reference, hypothesis string) (distance, runes int) {
	a := []rune(reference)
	b := []rune(hypothesis)
	runes = len(a)
	if len(a) == 0 {
		return len(b), 0
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 0
			if a[i-1] != b[j-1] {
				cost = 1
			}
			cur[j] = minInt(minInt(prev[j]+1, cur[j-1]+1), prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)], runes
}

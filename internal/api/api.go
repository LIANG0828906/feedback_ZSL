// Package api exposes the frozen pipeline result through a pure
// functional query interface: caption windows, per-character highlight
// intervals and the error-propagation report.
package api

import (
	"captionx/internal/model"
)

// Engine serves read-only queries over a computed result.
type Engine struct {
	res  *model.Result
	byID map[int]int // caption id -> index
}

// NewEngine wraps a computed result. The result is treated as immutable.
func NewEngine(res *model.Result) *Engine {
	e := &Engine{res: res, byID: map[int]int{}}
	for i, c := range res.Captions {
		e.byID[c.CaptionID] = i
	}
	return e
}

// CaptionWindow returns the display window of a caption.
func (e *Engine) CaptionWindow(captionID int) (model.Window, bool) {
	i, ok := e.byID[captionID]
	if !ok {
		return model.Window{}, false
	}
	return e.res.Captions[i].Window, true
}

// CharHighlight returns the highlight interval of one character.
func (e *Engine) CharHighlight(captionID, charIdx int) (model.Window, bool) {
	i, ok := e.byID[captionID]
	if !ok {
		return model.Window{}, false
	}
	c := e.res.Captions[i]
	if charIdx < 0 || charIdx >= len(c.CharStarts) {
		return model.Window{}, false
	}
	return model.Window{Start: c.CharStarts[charIdx], End: c.CharEnds[charIdx]}, true
}

// ErrorReport returns the full error-propagation ledger.
func (e *Engine) ErrorReport() model.ErrorReport {
	return e.res.Report
}

// Result returns the underlying frozen result (read-only).
func (e *Engine) Result() *model.Result { return e.res }

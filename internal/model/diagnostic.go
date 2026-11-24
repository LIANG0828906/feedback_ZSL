package model

import (
	"fmt"
	"strings"
)

const (
	CodeSchema  = "E_SCHEMA"
	CodeWeight  = "E_WEIGHT"
	CodeClip    = "E_CLIP"
	CodePara    = "E_PARA"
	CodeVersion = "E_VER"
	CodeScope   = "E_SCOPE"
	CodeDiff    = "E_DIFF"
	CodeSnap    = "E_SNAP"
	CodeRecover = "E_RECOVER"
	CodeGeom    = "E_GEOM"
	CodeKnot    = "E_KNOT"
	CodeStage   = "E_STAGE"
)

type PixelPoint struct {
	X int `json:"x"`
	Y int `json:"y"`
}

type Diagnostic struct {
	Code             string      `json:"code"`
	Message          string      `json:"message"`
	PatchID          int         `json:"patchId"`
	TileID           int         `json:"tileId"`
	Field            string      `json:"field"`
	VersionA         int64       `json:"versionA,omitempty"`
	VersionB         int64       `json:"versionB,omitempty"`
	ConflictingTiles []int       `json:"conflictingTiles,omitempty"`
	MislabeledTiles  []int       `json:"mislabeledTiles,omitempty"`
	Reasons          []string    `json:"reasons,omitempty"`
	FirstPixel       *PixelPoint `json:"firstPixel,omitempty"`
	Details          []string    `json:"details,omitempty"`
}

func (d *Diagnostic) Error() string {
	if d == nil {
		return "<nil>"
	}
	var b strings.Builder
	b.WriteString(d.Code)
	if d.PatchID >= 0 {
		fmt.Fprintf(&b, " patch=%d", d.PatchID)
	}
	if d.TileID >= 0 {
		fmt.Fprintf(&b, " tile=%d", d.TileID)
	}
	if d.Field != "" {
		fmt.Fprintf(&b, " field=%s", d.Field)
	}
	if d.Message != "" {
		fmt.Fprintf(&b, ": %s", d.Message)
	}
	return b.String()
}

func Diag(code, message string) *Diagnostic {
	return &Diagnostic{Code: code, Message: message, PatchID: -1, TileID: -1}
}

func DiagPatch(code string, patchID int, field, message string) *Diagnostic {
	return &Diagnostic{Code: code, PatchID: patchID, TileID: -1, Field: field, Message: message}
}

func DiagTile(code string, patchID, tileID int, field, message string) *Diagnostic {
	return &Diagnostic{Code: code, PatchID: patchID, TileID: tileID, Field: field, Message: message}
}

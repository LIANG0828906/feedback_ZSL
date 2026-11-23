package terrainbake

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"html"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DiagnosticInput is all data needed to render the standalone HTML report.
type DiagnosticInput struct {
	Title   string
	Project *Project
	Result  *BakeResult
	Outputs map[string]*BlockOutput
	Diff    *DiffReport
}

// WriteDiagnostics renders a self-contained HTML page plus four PNG atlases.
// It uses only the standard library and is safe to open offline.
func WriteDiagnostics(outDir string, in DiagnosticInput) (string, error) {
	if in.Project == nil {
		return "", &Error{Code: CodeParam, Object: "diagnostics", Field: "project", Message: "project is required"}
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", &Error{Code: CodeIO, Object: "diagnostics", Field: "out_dir", Message: err.Error()}
	}
	normalAtlas := renderNormalAtlas(in.Project, in.Outputs)
	colorAtlas := renderBlockColorAtlas(in.Project)
	seamAtlas := renderSeamHeatmap(in.Project, in.Result)
	diffAtlas := renderDiffHeatmap(in.Project, in.Outputs, in.Diff)

	images := []struct {
		name  string
		img   image.Image
		title string
	}{
		{"normals-atlas.png", normalAtlas, "Normal map atlas"},
		{"blocks-colored.png", colorAtlas, "Block shading preview"},
		{"seam-heatmap.png", seamAtlas, "Seam violation heatmap"},
		{"diff-heatmap.png", diffAtlas, "Difference heatmap"},
	}
	dataURIs := make(map[string]string, len(images))
	for _, item := range images {
		data, err := encodePNG(item.img)
		if err != nil {
			return "", &Error{Code: CodeIO, Object: "diagnostics", Field: item.name, Message: err.Error()}
		}
		if err := writeFileAtomic(filepath.Join(outDir, item.name), data, 0o644); err != nil {
			return "", &Error{Code: CodeIO, Object: "diagnostics", Field: item.name, Message: err.Error()}
		}
		dataURIs[item.name] = "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
	}
	page := renderDiagnosticsHTML(in, dataURIs)
	path := filepath.Join(outDir, "diagnostics.html")
	if err := writeFileAtomic(path, []byte(page), 0o644); err != nil {
		return "", &Error{Code: CodeIO, Object: "diagnostics", Field: "diagnostics.html", Message: err.Error()}
	}
	return path, nil
}

func renderNormalAtlas(p *Project, outputs map[string]*BlockOutput) *image.RGBA {
	w, h := projectExtent(p)
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 22, G: 24, B: 28, A: 255})
		}
	}
	for _, b := range p.Blocks {
		out := outputs[b.ID]
		for y := 0; y < b.Height; y++ {
			for x := 0; x < b.Width; x++ {
				var n Vec3
				if out != nil && y*out.Width+x < len(out.Normals) {
					n = out.Normals[y*out.Width+x]
				} else {
					n = Vec3{Z: 1}
				}
				r, g, bl := encodeNormal(n)
				img.SetRGBA(b.OriginX+x, b.OriginY+y, color.RGBA{R: r, G: g, B: bl, A: 255})
			}
		}
	}
	return img
}

func renderBlockColorAtlas(p *Project) *image.RGBA {
	w, h := projectExtent(p)
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 16, G: 18, B: 22, A: 255})
		}
	}
	for _, b := range p.Blocks {
		c := blockColor(b.ID)
		for y := 0; y < b.Height; y++ {
			for x := 0; x < b.Width; x++ {
				shade := uint8(0)
				if x == 0 || y == 0 || x == b.Width-1 || y == b.Height-1 {
					shade = 44
				}
				img.SetRGBA(b.OriginX+x, b.OriginY+y, color.RGBA{
					R: clampByte(int(c.R) + int(shade)),
					G: clampByte(int(c.G) + int(shade)),
					B: clampByte(int(c.B) + int(shade)), A: 255,
				})
			}
		}
	}
	return img
}

func renderSeamHeatmap(p *Project, res *BakeResult) *image.RGBA {
	w, h := projectExtent(p)
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for _, b := range p.Blocks {
		violations := 0
		if res != nil {
			if st, ok := res.SeamStats[b.ID]; ok {
				violations = st.Violations
			}
		}
		r, g, bl := uint8(45), uint8(190), uint8(105)
		if violations > 0 {
			r, g, bl = 255, 35, 25
		}
		for y := 0; y < b.Height; y++ {
			for x := 0; x < b.Width; x++ {
				img.SetRGBA(b.OriginX+x, b.OriginY+y, color.RGBA{R: r, G: g, B: bl, A: 255})
			}
		}
	}
	return img
}

func renderDiffHeatmap(p *Project, outputs map[string]*BlockOutput, diff *DiffReport) *image.RGBA {
	w, h := projectExtent(p)
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 14, G: 36, B: 24, A: 255})
		}
	}
	if diff == nil {
		return img
	}
	for _, b := range p.Blocks {
		mask := diff.DiffMask[b.ID]
		if len(mask) == 0 && b.ID == diff.BlockID && diff.Field == "normal_slope" {
			mask = make([]bool, b.Width*b.Height)
			if diff.Y >= 0 && diff.Y < b.Height && diff.X >= 0 && diff.X < b.Width {
				mask[diff.Y*b.Width+diff.X] = true
			}
		}
		for i := range mask {
			if !mask[i] {
				continue
			}
			x, y := i%b.Width, i/b.Width
			img.SetRGBA(b.OriginX+x, b.OriginY+y, color.RGBA{R: 255, G: 30, B: 30, A: 255})
		}
	}
	return img
}

func renderDiagnosticsHTML(in DiagnosticInput, images map[string]string) string {
	var b strings.Builder
	title := in.Title
	if title == "" {
		title = "Terrain normal bake diagnostics"
	}
	res := in.Result
	b.WriteString("<!doctype html><html lang=\"en\"><head><meta charset=\"utf-8\">")
	b.WriteString("<meta name=\"viewport\" content=\"width=device-width,initial-scale=1\">")
	b.WriteString("<title>" + html.EscapeString(title) + "</title>")
	b.WriteString(`<style>
:root{color-scheme:dark;--bg:#0f1115;--panel:#171b22;--line:#313845;--text:#e7edf7;--muted:#96a1b3;--good:#52d273;--bad:#ff5c5c;--warn:#ffbd4a}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--text);font:14px/1.45 ui-monospace,SFMono-Regular,Consolas,monospace}
main{max-width:1280px;margin:0 auto;padding:28px}h1{font-size:24px;margin:0 0 4px}h2{font-size:16px;margin:26px 0 10px;color:#cfd8e7}
.muted{color:var(--muted)}.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(240px,1fr));gap:12px}
.card{background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:14px}.metric{font-size:24px;font-weight:700}
.ok{color:var(--good)}.bad{color:var(--bad)}.warn{color:var(--warn)}
.atlas{display:grid;grid-template-columns:repeat(auto-fit,minmax(280px,1fr));gap:14px}.atlas figure{margin:0;background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:12px}
.atlas img{width:100%;image-rendering:pixelated;background:#0b0d10;border-radius:6px}.atlas figcaption{color:var(--muted);margin-top:8px}
table{width:100%;border-collapse:collapse;background:var(--panel);border:1px solid var(--line)}th,td{text-align:left;padding:8px 10px;border-bottom:1px solid var(--line);vertical-align:top}
th{color:#b9c6d8;font-weight:600}tr:last-child td{border-bottom:0}.pill{display:inline-block;padding:1px 7px;border-radius:999px;background:#242b36}
code{color:#c7d7ff}.chain{color:var(--muted);font-size:12px}.swatch{display:inline-block;width:12px;height:12px;border-radius:3px;margin-right:7px;vertical-align:-2px}
svg{width:100%;height:auto;background:var(--panel);border:1px solid var(--line);border-radius:10px}
footer{margin-top:28px;color:var(--muted);font-size:12px}
</style></head><body><main>`)
	b.WriteString("<h1>" + html.EscapeString(title) + "</h1>")
	b.WriteString("<div class=\"muted\">" + html.EscapeString(in.Project.Name) + " · seed " + fmt.Sprintf("%d", in.Project.Seed) + "</div>")

	baked, failed, paused := 0, 0, false
	if res != nil {
		baked = len(res.Baked)
		failed = len(res.Failed)
		paused = res.Paused
	}
	b.WriteString("<div class=\"grid\" style=\"margin-top:18px\">")
	writeMetric := func(label, value, class string) {
		b.WriteString("<div class=\"card\"><div class=\"muted\">" + label + "</div><div class=\"metric " + class + "\">" + value + "</div></div>")
	}
	writeMetric("Blocks", fmt.Sprintf("%d", len(in.Project.Blocks)), "")
	writeMetric("Baked this run", fmt.Sprintf("%d", baked), "ok")
	writeMetric("Failed blocks", fmt.Sprintf("%d", failed), boolClass(failed == 0, "ok", "bad"))
	writeMetric("Checkpoint", boolClass(!paused, "complete", "paused"), boolClass(!paused, "ok", "warn"))
	if in.Diff != nil {
		writeMetric("Snapshot diff", boolClass(in.Diff.Equal, "equal", "mismatch"), boolClass(in.Diff.Equal, "ok", "bad"))
	}
	b.WriteString("</div>")

	b.WriteString("<h2>Visual diagnostics</h2><div class=\"atlas\">")
	for _, item := range []struct{ file, cap string }{
		{"normals-atlas.png", "Final tangent-space normal atlas"},
		{"blocks-colored.png", "Block shading preview (stable ID colors)"},
		{"seam-heatmap.png", "Seam angle heatmap (red = at/over threshold)"},
		{"diff-heatmap.png", "Difference heatmap (red = differing pixel)"},
	} {
		b.WriteString("<figure><img alt=\"" + item.cap + "\" src=\"" + images[item.file] + "\"><figcaption>" + item.cap + "</figcaption></figure>")
	}
	b.WriteString("</div>")

	b.WriteString("<h2>Block and seam status</h2><table><thead><tr><th>Block</th><th></th><th>Size</th><th>Constrained pairs</th><th>Violations</th><th>Max angle</th><th>Digest</th></tr></thead><tbody>")
	for _, blk := range in.Project.Blocks {
		c := blockColor(blk.ID)
		st := SeamStats{}
		if res != nil {
			st = res.SeamStats[blk.ID]
		}
		digest := ""
		if out := in.Outputs[blk.ID]; out != nil {
			digest = shortHash(out.NormalDigest)
		}
		b.WriteString("<tr><td>" + html.EscapeString(blk.ID) + "</td><td><span class=\"swatch\" style=\"background:" + colorCSS(c) + "\"></span></td>")
		b.WriteString(fmt.Sprintf("<td>%dx%d</td><td>%d</td><td class=\"%s\">%d</td><td>%.6f°</td><td><code>%s</code></td></tr>",
			blk.Width, blk.Height, st.Constrained, boolClass(st.Violations == 0, "", "bad"), st.Violations, st.MaxAngleDeg, digest))
	}
	b.WriteString("</tbody></table>")

	if res != nil && len(res.Failed) > 0 {
		b.WriteString("<h2>Failed blocks</h2><table><thead><tr><th>Block</th><th>Failure</th><th>Retry</th></tr></thead><tbody>")
		ids := make([]string, 0, len(res.Failed))
		for id := range res.Failed {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			b.WriteString("<tr><td>" + html.EscapeString(id) + "</td><td class=\"bad\">" + html.EscapeString(res.Failed[id]) +
				"</td><td><code>terrainbake bake --mode resume ...</code></td></tr>")
		}
		b.WriteString("</tbody></table>")
	}

	b.WriteString("<h2>Invalidation propagation</h2>")
	b.WriteString(invalidationSVG(in.Project, in.Result))
	if res != nil && len(res.Invalidation) > 0 {
		b.WriteString("<table><thead><tr><th>Block</th><th>Reason</th><th>Dependency chain</th></tr></thead><tbody>")
		for _, item := range res.Invalidation {
			b.WriteString("<tr><td>" + html.EscapeString(item.BlockID) + "</td><td>" + html.EscapeString(item.Reason) + "</td><td class=\"chain\">" +
				html.EscapeString(strings.Join(item.Chain, " → ")) + "</td></tr>")
		}
		b.WriteString("</tbody></table>")
	} else {
		b.WriteString("<p class=\"muted\">No invalidated blocks in this run.</p>")
	}

	b.WriteString("<h2>Commit sequence</h2>")
	if res != nil {
		labels := make([]string, 0, len(res.CommitSequence))
		for _, ev := range res.CommitSequence {
			labels = append(labels, html.EscapeString(ev.BlockID)+" <span class=\"pill\">"+html.EscapeString(ev.Kind)+"</span>")
		}
		b.WriteString("<div class=\"card\">" + strings.Join(labels, " → ") + "</div>")
	}
	if res != nil && len(res.Queue) > 0 {
		b.WriteString("<h2>Resource queue events</h2><table><thead><tr><th>Block</th><th>Queue reason</th></tr></thead><tbody>")
		for _, q := range res.Queue {
			b.WriteString("<tr><td>" + html.EscapeString(q.BlockID) + "</td><td>" + html.EscapeString(q.Reason) + "</td></tr>")
		}
		b.WriteString("</tbody></table>")
	}
	b.WriteString("<footer>Algorithm " + html.EscapeString(AlgorithmVersion) + " · generated locally by terrainbake · no network assets</footer>")
	b.WriteString("</main></body></html>")
	return b.String()
}

func invalidationSVG(p *Project, res *BakeResult) string {
	w, h := projectExtent(p)
	scale := 8
	if w > 120 || h > 120 {
		scale = 4
	}
	vw, vh := w*scale+24, h*scale+24
	invalid := map[string]bool{}
	if res != nil {
		for _, item := range res.Invalidation {
			invalid[item.BlockID] = true
		}
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("<svg viewBox=\"0 0 %d %d\" role=\"img\" aria-label=\"Dependency graph\">", vw, vh))
	for _, s := range p.Seams {
		a, okA := FindBlock(p, s.BlockA)
		bb, okB := FindBlock(p, s.BlockB)
		if !okA || !okB {
			continue
		}
		x1 := 12 + (a.OriginX+a.Width/2)*scale
		y1 := 12 + (a.OriginY+a.Height/2)*scale
		x2 := 12 + (bb.OriginX+bb.Width/2)*scale
		y2 := 12 + (bb.OriginY+bb.Height/2)*scale
		stroke := "#4d5c72"
		if invalid[s.BlockA] || invalid[s.BlockB] {
			stroke = "#b84b4b"
		}
		b.WriteString(fmt.Sprintf("<line x1=\"%d\" y1=\"%d\" x2=\"%d\" y2=\"%d\" stroke=\"%s\" stroke-width=\"2\" stroke-dasharray=\"4 3\"/>", x1, y1, x2, y2, stroke))
	}
	for _, blk := range p.Blocks {
		x := 12 + blk.OriginX*scale
		y := 12 + blk.OriginY*scale
		ww := blk.Width * scale
		hh := blk.Height * scale
		stroke, fill := "#63738e", "#243044"
		if invalid[blk.ID] {
			stroke, fill = "#ff6868", "#4a2228"
		}
		b.WriteString(fmt.Sprintf("<rect x=\"%d\" y=\"%d\" width=\"%d\" height=\"%d\" rx=\"4\" fill=\"%s\" stroke=\"%s\" stroke-width=\"2\"/>", x, y, ww, hh, fill, stroke))
		b.WriteString(fmt.Sprintf("<text x=\"%d\" y=\"%d\" fill=\"#e7edf7\" font-size=\"%d\" font-family=\"monospace\">%s</text>",
			x+7, y+18, maxInt(11, scale+3), html.EscapeString(blk.ID)))
	}
	b.WriteString("</svg>")
	return b.String()
}

func projectExtent(p *Project) (int, int) {
	w, h := 1, 1
	for _, b := range p.Blocks {
		if b.OriginX+b.Width > w {
			w = b.OriginX + b.Width
		}
		if b.OriginY+b.Height > h {
			h = b.OriginY + b.Height
		}
	}
	return w, h
}

func blockColor(id string) color.RGBA {
	sum := sha256.Sum256([]byte(id))
	return color.RGBA{
		R: 55 + sum[0]%150,
		G: 55 + sum[1]%150,
		B: 55 + sum[2]%150,
		A: 255,
	}
}

func colorCSS(c color.RGBA) string {
	return fmt.Sprintf("#%02x%02x%02x", c.R, c.G, c.B)
}

func shortHash(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}

func clampByte(v int) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}

func boolClass(ok bool, good, bad string) string {
	if ok {
		return good
	}
	return bad
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

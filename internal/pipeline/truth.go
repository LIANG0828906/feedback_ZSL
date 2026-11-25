package pipeline

import (
	"math"

	"meshdoctor/internal/errs"
	"meshdoctor/internal/mesh"
	"meshdoctor/internal/sample"
)

func CheckTruth(m *mesh.Mesh, manifest *sample.Manifest) (*TruthReport, error) {
	report := &TruthReport{OK: true}

	for _, kp := range manifest.KnownPoints {
		distance, face := mesh.PointMeshDistance(kp.Position, m)
		tol := kp.Tolerance
		if tol <= 0 {
			tol = 1e-7
		}
		passed := distance <= tol
		report.Points = append(report.Points, PointCheck{
			PointID: kp.ID, Position: kp.Position, Distance: distance, Tolerance: tol,
			FaceIndex: face, Passed: passed,
		})
		if !passed {
			report.OK = false
			err := errs.New(errs.EPOINT, "known point %s distance %.9f exceeds tolerance %.9f", kp.ID, distance, tol)
			return report, errs.WithDetails(err, map[string]any{
				"point_id": kp.ID, "distance": distance, "tolerance": tol, "position": kp.Position,
			})
		}
	}

	for _, ring := range manifest.HoleRings {
		lc := checkRing(m, ring)
		report.Loops = append(report.Loops, lc)
		if !lc.Passed {
			report.OK = false
			err := errs.New(errs.ELOOP, "hole ring %s is broken, closure_error=%.9f tolerance=%.9f", ring.ID, lc.ClosureError, lc.Tolerance)
			return report, errs.WithDetails(err, map[string]any{
				"ring_id":          ring.ID,
				"ring_coordinates": ring.Coordinates,
				"closure_error":    lc.ClosureError,
				"worst_segment":    lc.WorstSegment,
				"worst_distance":   lc.WorstDistance,
			})
		}
	}

	return report, nil
}

func checkRing(m *mesh.Mesh, ring sample.HoleRing) LoopCheck {
	tol := ring.Tolerance
	if tol <= 0 {
		tol = 1e-7
	}
	lc := LoopCheck{RingID: ring.ID, Tolerance: tol, Coordinates: ring.Coordinates, Passed: true, WorstSegment: -1}
	if len(ring.Coordinates) < 3 {
		lc.Passed = false
		lc.ClosureError = math.Inf(1)
		return lc
	}

	samples := 8
	for i := 0; i < len(ring.Coordinates); i++ {
		a := ring.Coordinates[i]
		b := ring.Coordinates[(i+1)%len(ring.Coordinates)]
		for s := 0; s <= samples; s++ {
			t := float64(s) / float64(samples)
			p := mesh.Add(mesh.Scale(a, 1-t), mesh.Scale(b, t))
			d, _ := mesh.PointMeshDistance(p, m)
			if d > lc.WorstDistance {
				lc.WorstDistance = d
				lc.WorstSegment = i
			}
		}
	}
	lc.ClosureError = lc.WorstDistance
	lc.Passed = lc.ClosureError <= tol
	return lc
}

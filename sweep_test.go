package grid

import "testing"

func TestSweepHitNegativeDenominatorSegmentBounds(t *testing.T) {
	vertical := []Obstacle{{ID: "V", A: Vec2{X: 0, Y: 0}, B: Vec2{X: 0, Y: 10}}}

	hit := findSweepHit(Vec2{X: -5, Y: 5}, Vec2{X: 5, Y: 5}, vertical)
	if hit == nil || hit.obstacle.ID != "V" {
		t.Fatalf("expected interior vertical hit, got %#v", hit)
	}

	miss := findSweepHit(Vec2{X: -5, Y: -10}, Vec2{X: 5, Y: -10}, vertical)
	if miss != nil {
		t.Fatalf("intersection outside segment must be rejected, got %#v", miss)
	}
}

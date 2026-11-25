package incremental

import (
	"fmt"

	"maintenanceorchestrator/internal/domain"
)

func PatchNodeWatermark(p domain.Project, nodeID string, basisPoints int) (domain.Project, Change, error) {
	cp := domain.CloneProject(p)
	for i := range cp.Nodes {
		if cp.Nodes[i].ID != nodeID {
			continue
		}
		if basisPoints < 0 {
			return cp, Change{}, domain.NewError(domain.ENegativeWatermark,
				fmt.Sprintf("$.nodes[%d].watermark_basis_points", i),
				"watermark must not be negative: %d", basisPoints)
		}
		cp.Nodes[i].WatermarkBasisPoints = basisPoints
		return cp, Change{
			Source: "node:" + nodeID,
			Path:   fmt.Sprintf("$.nodes[%d].watermark_basis_points", i),
			Reason: "node watermark changed",
		}, nil
	}
	return cp, Change{}, domain.NewError(domain.EInvalidCapacity, "$.nodes", "unknown node %q", nodeID)
}

func PatchWindow(p domain.Project, windowID string, start, end domain.Tick) (domain.Project, Change, error) {
	cp := domain.CloneProject(p)
	if end <= start {
		return cp, Change{}, domain.NewError(domain.EInvalidWindow, "$.window", "window end must be after start")
	}
	if windowID == "global" || windowID == "" {
		cp.Window.Start = start
		cp.Window.End = end
		return cp, Change{Source: "window:global", Path: "$.window", Reason: "global maintenance window changed"}, nil
	}
	for i := range cp.Nodes {
		if cp.Nodes[i].ID == windowID {
			cp.Nodes[i].Window = domain.Window{Start: start, End: end}
			return cp, Change{Source: "window:" + windowID, Path: fmt.Sprintf("$.nodes[%d].window", i), Reason: "node window changed"}, nil
		}
	}
	return cp, Change{}, domain.NewError(domain.EInvalidWindow, "$.window", "unknown window %q", windowID)
}

func PatchLeaseExpiry(p domain.Project, leaseID string, expiry domain.Tick) (domain.Project, Change, error) {
	cp := domain.CloneProject(p)
	for i := range cp.Loans {
		if cp.Loans[i].ID != leaseID {
			continue
		}
		if expiry < cp.Loans[i].StartTick {
			return cp, Change{}, domain.NewError(domain.ELoanStartAfterExpiry,
				fmt.Sprintf("$.loans[%d].expiry_tick", i),
				"expiry %d is before start %d", expiry, cp.Loans[i].StartTick)
		}
		cp.Loans[i].ExpiryTick = expiry
		return cp, Change{
			Source: "lease:" + leaseID,
			Path:   fmt.Sprintf("$.loans[%d].expiry_tick", i),
			Reason: "lease expiry changed",
		}, nil
	}
	return cp, Change{}, domain.NewError(domain.EInvalidCapacity, "$.loans", "unknown lease %q", leaseID)
}

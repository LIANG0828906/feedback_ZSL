package graph

import (
	"sort"

	"lipsync/domain"
)

type Graph struct {
	project        *domain.Project
	packages       map[string]domain.ScheduledPackage
	packageByPhone map[string][]string
	edges          map[string][]string
}

func Build(p *domain.Project, sched *domain.ScheduleResult) *Graph {
	g := &Graph{
		project:        p,
		packages:       make(map[string]domain.ScheduledPackage),
		packageByPhone: make(map[string][]string),
		edges:          make(map[string][]string),
	}
	for _, pkg := range sched.Packages {
		g.packages[pkg.ID] = pkg
		pnode := packageNode(pkg.ID)
		for _, phone := range pkg.PhonemeIDs {
			phNode := phonemeNode(pkg.DialogueID, phone)
			g.packageByPhone[phKey(pkg.DialogueID, phone)] = append(
				g.packageByPhone[phKey(pkg.DialogueID, phone)], pkg.ID)
			g.edges[phNode] = appendUnique(g.edges[phNode], pnode)
		}
		if pkg.Status == "accepted" {
			for f := pkg.DispatchStartFrame; f < pkg.DispatchEndFrame; f++ {
				fnode := frameNode(pkg.ID, f)
				g.edges[pnode] = appendUnique(g.edges[pnode], fnode)
			}
		}
	}
	// Explicit downstream scheduling edges: a package can affect later
	// reservations on its own channel within the retry horizon, and can affect
	// globally overlapping packages through the per-frame budget.
	for _, from := range sched.Packages {
		fromEnd := from.EndFrame
		if from.DispatchEndFrame > fromEnd {
			fromEnd = from.DispatchEndFrame
		}
		horizon := fromEnd + p.Render.MaxDelayFrames
		for _, to := range sched.Packages {
			if from.ID == to.ID {
				continue
			}
			sameChannel := from.Channel == to.Channel && to.StartFrame <= horizon &&
				to.EndFrame >= from.StartFrame
			globalOverlap := to.StartFrame < from.EndFrame && to.EndFrame > from.StartFrame
			if sameChannel || globalOverlap {
				g.edges[packageNode(from.ID)] = appendUnique(
					g.edges[packageNode(from.ID)], packageNode(to.ID))
			}
		}
	}
	for k := range g.edges {
		sort.Strings(g.edges[k])
	}
	return g
}

func (g *Graph) Edges() map[string][]string {
	out := make(map[string][]string, len(g.edges))
	for k, v := range g.edges {
		out[k] = append([]string(nil), v...)
	}
	return out
}

// Invalidate marks the edited phoneme, all packages whose partition or
// scheduling can be affected, and their dispatch-frame nodes. Other package
// nodes remain reusable.
func (g *Graph) Invalidate(edit domain.Edit) (*domain.Invalidation, error) {
	inv := &domain.Invalidation{Edit: edit}
	affected := make(map[string]struct{})
	packagesByID := make(map[string]domain.ScheduledPackage, len(g.packages))
	for id, pkg := range g.packages {
		packagesByID[id] = pkg
	}

	switch edit.Kind {
	case domain.EditFPS:
		inv.Reason = "frame-rate edit changes quantization for the whole project"
		for phoneKey := range g.packageByPhone {
			dialogue, phone := splitPhoneKey(phoneKey)
			inv.PhonemeNodes = append(inv.PhonemeNodes, phonemeNode(dialogue, phone))
		}
		for id, pkg := range g.packages {
			affected[id] = struct{}{}
			if pkg.Status == "accepted" {
				for f := pkg.DispatchStartFrame; f < pkg.DispatchEndFrame; f++ {
					inv.FrameNodes = append(inv.FrameNodes, frameNode(id, f))
				}
			}
		}
	case domain.EditBoundary, domain.EditWeight:
		dialogueFound := false
		for _, d := range g.project.Dialogues {
			if d.ID == edit.DialogueID || (edit.DialogueID == "" && d.ShardIndex == edit.ShardIndex) {
				dialogueFound = true
				break
			}
		}
		if !dialogueFound {
			return nil, &domain.Error{
				Code: domain.CodeParam, Field: "edit.dialogue_id",
				DialogueID: edit.DialogueID, ShardIndex: edit.ShardIndex,
				Message: "edited dialogue not found",
			}
		}
		key := phKey(edit.DialogueID, edit.PhonemeID)
		pkgIDs := g.packageByPhone[key]
		if len(pkgIDs) == 0 {
			return nil, &domain.Error{
				Code: domain.CodeParam, Field: "edit.phoneme_id",
				DialogueID: edit.DialogueID, ShardIndex: edit.ShardIndex,
				PhonemeID: edit.PhonemeID, Message: "edited phoneme not found in dependency graph",
			}
		}
		inv.PhonemeNodes = append(inv.PhonemeNodes, phonemeNode(edit.DialogueID, edit.PhonemeID))
		if edit.Kind == domain.EditBoundary {
			inv.Reason = "phoneme boundary edit invalidates its package and downstream timeline packages"
			earliest := int64(1<<62 - 1)
			for _, id := range pkgIDs {
				affected[id] = struct{}{}
				if p := packagesByID[id]; p.StartFrame < earliest {
					earliest = p.StartFrame
				}
			}
			for id, pkg := range g.packages {
				if pkg.DialogueID == edit.DialogueID && pkg.StartFrame >= earliest {
					affected[id] = struct{}{}
				}
			}
		} else {
			inv.Reason = "duration-weight edit invalidates its package and dependent dispatch downstream"
			for _, id := range pkgIDs {
				affected[id] = struct{}{}
			}
		}
	default:
		return nil, &domain.Error{
			Code: domain.CodeParam, Field: "edit.kind", Value: string(edit.Kind),
			Message: "unknown edit kind",
		}
	}

	// Propagate along explicit downstream package edges, then include channels
	// and globally overlapping reservations that can change materially.
	queue := make([]string, 0, len(affected))
	for id := range affected {
		queue = append(queue, packageNode(id))
	}
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		for _, next := range g.edges[node] {
			if !isPackageNode(next) {
				continue
			}
			id := trimPackageNode(next)
			if _, ok := affected[id]; ok {
				continue
			}
			affected[id] = struct{}{}
			queue = append(queue, next)
		}
	}

	for id := range affected {
		pkg, ok := g.packages[id]
		if !ok {
			continue
		}
		inv.PackageNodes = append(inv.PackageNodes, packageNode(id))
		inv.PackageIDs = append(inv.PackageIDs, id)
		if pkg.Status == "accepted" {
			for f := pkg.DispatchStartFrame; f < pkg.DispatchEndFrame; f++ {
				inv.FrameNodes = append(inv.FrameNodes, frameNode(id, f))
			}
		}
	}
	for id := range g.packages {
		if _, ok := affected[id]; !ok {
			inv.ReusedPackages = append(inv.ReusedPackages, id)
		}
	}
	sort.Strings(inv.PhonemeNodes)
	sort.Strings(inv.PackageNodes)
	sort.Strings(inv.FrameNodes)
	sort.Strings(inv.PackageIDs)
	sort.Strings(inv.ReusedPackages)
	return inv, nil
}

func phonemeNode(dialogue, phone string) string {
	return "phoneme:" + dialogue + ":" + phone
}

func packageNode(id string) string { return "package:" + id }

func frameNode(packageID string, frame int64) string {
	return "dispatch:" + packageID + ":" + domainFormatInt(frame)
}

func isPackageNode(s string) bool {
	return len(s) > len("package:") && s[:len("package:")] == "package:"
}

func trimPackageNode(s string) string { return s[len("package:"):] }

func domainFormatInt(v int64) string {
	// local formatting avoids a fmt dependency in hot graph paths.
	if v == 0 {
		return "0"
	}
	var buf [24]byte
	i := len(buf)
	neg := v < 0
	if neg {
		v = -v
	}
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func appendUnique(xs []string, v string) []string {
	for _, x := range xs {
		if x == v {
			return xs
		}
	}
	return append(xs, v)
}

func phKey(dialogue, phone string) string { return dialogue + "\x00" + phone }

func splitPhoneKey(key string) (string, string) {
	for i := 0; i < len(key); i++ {
		if key[i] == 0 {
			return key[:i], key[i+1:]
		}
	}
	return key, ""
}

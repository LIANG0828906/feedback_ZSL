package level

// enforceBudget applies the declared tradeoff strategy until the
// partition difficulty falls inside [BudgetMin, BudgetMax]:
//  1. over budget: replace the most expensive active obstacle with the
//     cheapest reserve candidate when that lowers the cost;
//  2. else prune the most expensive active obstacle;
//  3. else prune a resource from the currently most represented category
//     (keeps the category distribution deviation bounded);
//  4. under budget: add back the most expensive reserve candidate.
//
// Every step is recorded. If the budget cannot be satisfied within the
// declared iteration limit, E_BUDGET is returned with the current
// difficulty and the budget range.
func enforceBudget(proj *Project, pc *PartitionConstraint, res *PartitionResult) error {
	iters := 0
	for {
		d := DifficultyOf(res)
		if d > pc.BudgetMax {
			if iters >= proj.MaxBudgetIters {
				return budgetErr(pc.ID, d, pc.BudgetMin, pc.BudgetMax)
			}
			iters++
			if !reduceOnce(pc, res) {
				return budgetErr(pc.ID, d, pc.BudgetMin, pc.BudgetMax)
			}
			continue
		}
		if d < pc.BudgetMin {
			if iters >= proj.MaxBudgetIters {
				return budgetErr(pc.ID, d, pc.BudgetMin, pc.BudgetMax)
			}
			iters++
			if !raiseOnce(pc, res) {
				return budgetErr(pc.ID, d, pc.BudgetMin, pc.BudgetMax)
			}
			continue
		}
		res.Difficulty = d
		recomputePassable(res)
		return nil
	}
}

func highestCostIdx(obs []Obstacle) int {
	best := -1
	for i, ob := range obs {
		if best < 0 || ob.Cost > obs[best].Cost || (ob.Cost == obs[best].Cost && ob.ID < obs[best].ID) {
			best = i
		}
	}
	return best
}

func cheapestIdx(obs []Obstacle) int {
	best := -1
	for i, ob := range obs {
		if best < 0 || ob.Cost < obs[best].Cost || (ob.Cost == obs[best].Cost && ob.ID < obs[best].ID) {
			best = i
		}
	}
	return best
}

func reduceOnce(pc *PartitionConstraint, res *PartitionResult) bool {
	hi := highestCostIdx(res.Obstacles)
	if hi >= 0 {
		cheap := cheapestIdx(res.Reserve)
		if cheap >= 0 && res.Reserve[cheap].Cost < res.Obstacles[hi].Cost {
			removed := res.Obstacles[hi]
			res.Obstacles[hi] = res.Reserve[cheap]
			res.Reserve = append(res.Reserve[:cheap], res.Reserve[cheap+1:]...)
			res.Reserve = append(res.Reserve, removed)
			res.Tradeoffs = append(res.Tradeoffs, TradeoffRecord{
				Partition: pc.ID, CandidateID: removed.ID,
				Action: "replace", Reason: "over_budget_replace_with_cheaper_reserve",
				DifficultyAfter: DifficultyOf(res),
			})
			return true
		}
		removed := res.Obstacles[hi]
		res.Obstacles = append(res.Obstacles[:hi], res.Obstacles[hi+1:]...)
		res.Reserve = append(res.Reserve, removed)
		res.Tradeoffs = append(res.Tradeoffs, TradeoffRecord{
			Partition: pc.ID, CandidateID: removed.ID,
			Action: "prune", Reason: "over_budget_prune_highest_cost",
			DifficultyAfter: DifficultyOf(res),
		})
		return true
	}
	if len(res.Resources) > 0 {
		removed := pruneResource(res)
		res.Tradeoffs = append(res.Tradeoffs, TradeoffRecord{
			Partition: pc.ID, CandidateID: removed.ID,
			Action: "prune_resource", Reason: "over_budget_prune_resource_largest_category",
			DifficultyAfter: DifficultyOf(res),
		})
		return true
	}
	return false
}

func raiseOnce(pc *PartitionConstraint, res *PartitionResult) bool {
	hi := highestCostIdx(res.Reserve)
	if hi < 0 {
		return false
	}
	added := res.Reserve[hi]
	res.Reserve = append(res.Reserve[:hi], res.Reserve[hi+1:]...)
	res.Obstacles = append(res.Obstacles, added)
	res.Tradeoffs = append(res.Tradeoffs, TradeoffRecord{
		Partition: pc.ID, CandidateID: added.ID,
		Action: "add", Reason: "under_budget_add_highest_cost_reserve",
		DifficultyAfter: DifficultyOf(res),
	})
	return true
}

// pruneResource removes one resource from the currently most represented
// category, which keeps the per-category share deviation bounded by one.
func pruneResource(res *PartitionResult) Resource {
	counts := map[int]int{}
	for _, r := range res.Resources {
		counts[r.Category]++
	}
	bestCat, bestCount := -1, -1
	for cat, n := range counts {
		if n > bestCount || (n == bestCount && cat < bestCat) {
			bestCat, bestCount = cat, n
		}
	}
	for i := len(res.Resources) - 1; i >= 0; i-- {
		if res.Resources[i].Category == bestCat {
			removed := res.Resources[i]
			res.Resources = append(res.Resources[:i], res.Resources[i+1:]...)
			return removed
		}
	}
	return Resource{}
}

package balance

import "fmt"

type Injection struct {
	Kind       string
	PlanID     string
	ScenarioID string
	MatchIndex int
	Turn       int
}

type combatant struct {
	buildID     string
	side        string
	hp          Fixed
	maxHP       Fixed
	resource    Fixed
	maxResource Fixed
	regen       Fixed
	skills      []skillModel
	cooldowns   map[string]int
	dead        bool
	reviveAt    int
}

func simulateMatch(
	seed int64,
	builds map[string]Build,
	scenario Scenario,
	prepared *preparedPlan,
	matchIndex int,
	injection Injection,
	attempt int,
) (*MatchReport, error) {
	left, err := makeTeam("left", scenario.LeftBuilds, builds, prepared)
	if err != nil {
		return nil, err
	}
	right, err := makeTeam("right", scenario.RightBuilds, builds, prepared)
	if err != nil {
		return nil, err
	}

	instanceID := fmt.Sprintf("%s#%d", scenario.ID, matchIndex)
	leftDamage := make([]Fixed, 0, scenario.MaxRounds)
	rightDamage := make([]Fixed, 0, scenario.MaxRounds)
	var (
		totalLeft, totalRight              Fixed
		minPossibleLeft, maxPossibleLeft   Fixed
		minPossibleRight, maxPossibleRight Fixed
		revivesLeft, revivesRight          int
		actionTurn                         int
		winner                             string
		roundsPlayed                       int
	)

	for roundNumber := 1; roundNumber <= scenario.MaxRounds; roundNumber++ {
		for _, unit := range append(append([]*combatant{}, left...), right...) {
			if unit.dead {
				if scenario.ReviveRounds > 0 && unit.reviveAt > 0 && unit.reviveAt <= roundNumber {
					unit.dead = false
					unit.reviveAt = 0
					unit.hp = unit.maxHP / 2
					if unit.hp < 1 {
						unit.hp = 1
					}
					unit.resource = unit.maxResource / 2
					unit.cooldowns = make(map[string]int)
					if unit.side == "left" {
						revivesLeft++
					} else {
						revivesRight++
					}
				}
				continue
			}
			unit.resource += unit.regen
			if unit.resource > unit.maxResource {
				unit.resource = unit.maxResource
			}
		}

		roundDamageLeft := Fixed(0)
		roundDamageRight := Fixed(0)
		roundsPlayed = roundNumber

		for _, actors := range [][]*combatant{left, right} {
			for _, attacker := range actors {
				if attacker.dead {
					continue
				}
				opponents := right
				if attacker.side == "right" {
					opponents = left
				}
				if teamEliminated(opponents, scenario) {
					break
				}

				skill, hasSkill := selectSkill(attacker, roundNumber)
				actionTurn++
				if injectionMatches(injection, prepared.Plan.ID, scenario.ID, matchIndex) && injection.Turn == actionTurn {
					if err := injectedActionError(injection, prepared.Plan.ID, scenario.ID, matchIndex, actionTurn, attempt, attacker, skill, hasSkill); err != nil {
						return nil, err
					}
				}
				if !hasSkill {
					continue
				}

				target := firstLiving(opponents)
				if target == nil {
					break
				}
				roll := deriveRoll(seed, instanceID, actionTurn)
				span := skill.HitMaxBPS - skill.HitMinBPS
				varianceBPS := skill.HitMinBPS
				if span > 0 {
					varianceBPS += (span * roll) / 9999
				}
				damage, ok := damageWithVariance(skill.BaseDamage, varianceBPS)
				if !ok {
					return nil, &AppError{
						Code:       CodeRange,
						Object:     skill.SkillID,
						PlanID:     prepared.Plan.ID,
						ScenarioID: scenario.ID,
						MatchIndex: matchIndex,
						Turn:       actionTurn,
						Detail:     "hit damage overflow",
					}
				}

				target.hp -= damage
				if target.hp <= 0 {
					target.hp = 0
					target.dead = true
					if scenario.ReviveRounds > 0 {
						target.reviveAt = roundNumber + scenario.ReviveRounds
					}
				}
				if attacker.side == "left" {
					roundDamageLeft += damage
					totalLeft += damage
					minPossibleLeft += skill.MinDamage
					maxPossibleLeft += skill.MaxDamage
				} else {
					roundDamageRight += damage
					totalRight += damage
					minPossibleRight += skill.MinDamage
					maxPossibleRight += skill.MaxDamage
				}
				attacker.resource -= skill.ResourceCost
				if attacker.resource < 0 {
					return nil, &AppError{
						Code:       CodeResource,
						Object:     skill.SkillID,
						PlanID:     prepared.Plan.ID,
						ScenarioID: scenario.ID,
						MatchIndex: matchIndex,
						Turn:       actionTurn,
						Detail:     "resource became negative after settlement",
					}
				}
				attacker.cooldowns[skill.SkillID] = roundNumber + skill.Cooldown + 1
			}
		}

		leftDamage = append(leftDamage, roundDamageLeft)
		rightDamage = append(rightDamage, roundDamageRight)

		leftEliminated := teamEliminated(left, scenario)
		rightEliminated := teamEliminated(right, scenario)
		switch {
		case leftEliminated && rightEliminated:
			winner = "draw"
		case leftEliminated:
			winner = "right"
		case rightEliminated:
			winner = "left"
		}
		if winner != "" {
			break
		}
	}

	if winner == "" {
		winner = decideAtRoundLimit(left, right, totalLeft, totalRight)
	}

	match := &MatchReport{
		MatchIndex:       matchIndex,
		Attempts:         attempt,
		RoundsPlayed:     roundsPlayed,
		ActionTurns:      actionTurn,
		Winner:           winner,
		DamageLeft:       leftDamage,
		DamageRight:      rightDamage,
		TotalLeft:        totalLeft,
		TotalRight:       totalRight,
		MinPossibleLeft:  minPossibleLeft,
		MaxPossibleLeft:  maxPossibleLeft,
		MinPossibleRight: minPossibleRight,
		MaxPossibleRight: maxPossibleRight,
		RevivesLeft:      revivesLeft,
		RevivesRight:     revivesRight,
	}
	match.Digest = buildMatchDigest(match)
	return match, nil
}

func makeTeam(side string, buildIDs []string, builds map[string]Build, prepared *preparedPlan) ([]*combatant, error) {
	team := make([]*combatant, 0, len(buildIDs))
	for _, buildID := range buildIDs {
		build, ok := builds[buildID]
		if !ok {
			return nil, &AppError{
				Code:   CodeUnit,
				Object: buildID,
				PlanID: prepared.Plan.ID,
				Field:  "scenario.builds",
				Detail: "scenario build is missing",
			}
		}
		models := prepared.BuildSkills[buildID]
		skills := make([]skillModel, 0, len(build.Rotation))
		for _, skillID := range build.Rotation {
			model, exists := models[skillID]
			if !exists {
				return nil, &AppError{
					Code:   CodeUnit,
					Object: skillID,
					PlanID: prepared.Plan.ID,
					Field:  "build.rotation",
					Detail: "build skill is missing from prepared plan",
				}
			}
			skills = append(skills, model)
		}
		team = append(team, &combatant{
			buildID:     buildID,
			side:        side,
			hp:          build.MaxHP,
			maxHP:       build.MaxHP,
			resource:    build.MaxResource,
			maxResource: build.MaxResource,
			regen:       build.ResourceRegen,
			skills:      skills,
			cooldowns:   make(map[string]int),
		})
	}
	return team, nil
}

func selectSkill(unit *combatant, round int) (skillModel, bool) {
	for _, skill := range unit.skills {
		if unit.cooldowns[skill.SkillID] > round {
			continue
		}
		if unit.resource < skill.ResourceCost {
			continue
		}
		return skill, true
	}
	return skillModel{}, false
}

func firstLiving(team []*combatant) *combatant {
	for _, unit := range team {
		if !unit.dead {
			return unit
		}
	}
	return nil
}

func teamEliminated(team []*combatant, scenario Scenario) bool {
	for _, unit := range team {
		if !unit.dead {
			return false
		}
		if scenario.ReviveRounds > 0 && unit.reviveAt > 0 && unit.reviveAt <= scenario.MaxRounds {
			return false
		}
	}
	return true
}

func decideAtRoundLimit(left, right []*combatant, totalLeft, totalRight Fixed) string {
	leftAlive := aliveCount(left)
	rightAlive := aliveCount(right)
	if leftAlive != rightAlive {
		if leftAlive > rightAlive {
			return "left"
		}
		return "right"
	}
	leftHP := totalHP(left)
	rightHP := totalHP(right)
	if leftHP != rightHP {
		if leftHP > rightHP {
			return "left"
		}
		return "right"
	}
	if totalLeft != totalRight {
		if totalLeft > totalRight {
			return "left"
		}
		return "right"
	}
	return "draw"
}

func aliveCount(team []*combatant) int {
	count := 0
	for _, unit := range team {
		if !unit.dead {
			count++
		}
	}
	return count
}

func totalHP(team []*combatant) Fixed {
	var total Fixed
	for _, unit := range team {
		if !unit.dead {
			total += unit.hp
		}
	}
	return total
}

func injectionMatches(injection Injection, planID, scenarioID string, matchIndex int) bool {
	if injection.Kind == "" {
		return false
	}
	if injection.PlanID != "" && injection.PlanID != planID {
		return false
	}
	if injection.ScenarioID != "" && injection.ScenarioID != scenarioID {
		return false
	}
	if injection.MatchIndex > 0 && injection.MatchIndex != matchIndex {
		return false
	}
	return true
}

func injectedActionError(
	injection Injection,
	planID, scenarioID string,
	matchIndex, turn, attempt int,
	attacker *combatant,
	skill skillModel,
	hasSkill bool,
) error {
	object := "__injected_missing_skill__"
	if hasSkill {
		object = skill.SkillID
	} else if len(attacker.skills) > 0 {
		object = attacker.skills[0].SkillID
	}
	switch injection.Kind {
	case "unit":
		return &AppError{
			Code:       CodeUnit,
			Object:     object,
			PlanID:     planID,
			ScenarioID: scenarioID,
			MatchIndex: matchIndex,
			Turn:       turn,
			Field:      "skill.reference",
			Detail:     "injected missing skill reference",
		}
	case "cooldown":
		return &AppError{
			Code:       CodeCooldown,
			Object:     object,
			PlanID:     planID,
			ScenarioID: scenarioID,
			MatchIndex: matchIndex,
			Turn:       turn,
			Field:      "skill.cooldown",
			Detail:     "injected cast while cooldown was still active",
		}
	case "resource":
		return &AppError{
			Code:       CodeResource,
			Object:     object,
			PlanID:     planID,
			ScenarioID: scenarioID,
			MatchIndex: matchIndex,
			Turn:       turn,
			Field:      "skill.resourceCost",
			Detail:     "injected cast without enough resources",
		}
	case "internal":
		if attempt <= 1 {
			return &AppError{
				Code:       CodeInternal,
				Object:     "simulation",
				PlanID:     planID,
				ScenarioID: scenarioID,
				MatchIndex: matchIndex,
				Turn:       turn,
				Detail:     "injected transient internal simulation error",
			}
		}
	}
	return nil
}

package balance

import "sync"

type PlanState string
type ComboState string

const (
	PlanPending   PlanState = "pending"
	PlanRunning   PlanState = "running"
	PlanCommitted PlanState = "committed"
	PlanFailed    PlanState = "failed"

	ComboPending   ComboState = "pending"
	ComboRunning   ComboState = "running"
	ComboCommitted ComboState = "committed"
	ComboFailed    ComboState = "failed"
)

type StateSnapshot struct {
	Plans  map[string]PlanState  `json:"plans"`
	Combos map[string]ComboState `json:"combos"`
}

type StateTracker struct {
	mu           sync.Mutex
	plans        map[string]PlanState
	combos       map[string]ComboState
	planPayload  map[string]any
	comboPayload map[string]any
}

func NewStateTracker(project *Project) *StateTracker {
	tracker := &StateTracker{
		plans:        make(map[string]PlanState, len(project.Plans)),
		combos:       make(map[string]ComboState),
		planPayload:  make(map[string]any),
		comboPayload: make(map[string]any),
	}
	for _, plan := range project.Plans {
		tracker.plans[plan.ID] = PlanPending
		for _, scenarioID := range plan.TargetScenarios {
			key := ComboID{PlanID: plan.ID, ScenarioID: scenarioID}.Key()
			tracker.combos[key] = ComboPending
		}
	}
	return tracker
}

func (s *StateTracker) TransitionPlan(planID string, to PlanState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transitionPlanLocked(planID, to)
}

func (s *StateTracker) transitionPlanLocked(planID string, to PlanState) error {
	from, ok := s.plans[planID]
	if !ok {
		return &AppError{Code: CodeState, Object: planID, PlanID: planID, Field: "state", Detail: "unknown plan"}
	}
	if !legalPlanTransition(from, to) {
		return &AppError{
			Code:     CodeState,
			Object:   planID,
			PlanID:   planID,
			Field:    "state",
			Actual:   string(from),
			Expected: string(to),
			Detail:   "illegal plan state transition",
		}
	}
	s.plans[planID] = to
	return nil
}

func (s *StateTracker) TransitionCombo(planID, scenarioID string, to ComboState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transitionComboLocked(planID, scenarioID, to)
}

func (s *StateTracker) transitionComboLocked(planID, scenarioID string, to ComboState) error {
	key := ComboID{PlanID: planID, ScenarioID: scenarioID}.Key()
	from, ok := s.combos[key]
	if !ok {
		return &AppError{
			Code:       CodeState,
			Object:     key,
			PlanID:     planID,
			ScenarioID: scenarioID,
			Field:      "state",
			Detail:     "unknown plan/scenario combination",
		}
	}
	if !legalComboTransition(from, to) {
		return &AppError{
			Code:       CodeState,
			Object:     key,
			PlanID:     planID,
			ScenarioID: scenarioID,
			Field:      "state",
			Actual:     string(from),
			Expected:   string(to),
			Detail:     "illegal combination state transition",
		}
	}
	s.combos[key] = to
	return nil
}

func legalPlanTransition(from, to PlanState) bool {
	switch from {
	case PlanPending:
		return to == PlanRunning
	case PlanRunning:
		return to == PlanCommitted || to == PlanFailed
	case PlanFailed:
		return to == PlanRunning
	default:
		return false
	}
}

func legalComboTransition(from, to ComboState) bool {
	switch from {
	case ComboPending:
		return to == ComboRunning
	case ComboRunning:
		return to == ComboCommitted || to == ComboFailed
	case ComboFailed:
		return to == ComboRunning
	default:
		return false
	}
}

func (s *StateTracker) CommitPlan(planID string, payload any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.transitionPlanLocked(planID, PlanCommitted); err != nil {
		return err
	}
	s.planPayload[planID] = payload
	return nil
}

func (s *StateTracker) CommitCombo(planID, scenarioID string, payload any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.transitionComboLocked(planID, scenarioID, ComboCommitted); err != nil {
		return err
	}
	key := ComboID{PlanID: planID, ScenarioID: scenarioID}.Key()
	s.comboPayload[key] = payload
	return nil
}

func (s *StateTracker) RestoreCommittedCombo(planID, scenarioID string, payload any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := ComboID{PlanID: planID, ScenarioID: scenarioID}.Key()
	if _, ok := s.combos[key]; !ok {
		return &AppError{
			Code:       CodeState,
			Object:     key,
			PlanID:     planID,
			ScenarioID: scenarioID,
			Field:      "state",
			Detail:     "cannot restore unknown combination",
		}
	}
	s.combos[key] = ComboCommitted
	s.comboPayload[key] = payload
	return nil
}

func (s *StateTracker) RestoreCommittedPlan(planID string, payload any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.plans[planID]; !ok {
		return &AppError{Code: CodeState, Object: planID, PlanID: planID, Field: "state", Detail: "cannot restore unknown plan"}
	}
	s.plans[planID] = PlanCommitted
	s.planPayload[planID] = payload
	return nil
}

// SetComboPayload is intentionally rejected after a combination is committed.
// It exists to make the freeze invariant executable and testable.
func (s *StateTracker) SetComboPayload(planID, scenarioID string, payload any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := ComboID{PlanID: planID, ScenarioID: scenarioID}.Key()
	state, ok := s.combos[key]
	if !ok {
		return &AppError{Code: CodeState, Object: key, PlanID: planID, ScenarioID: scenarioID, Field: "state", Detail: "unknown combination"}
	}
	if state == ComboCommitted {
		return &AppError{
			Code:       CodeState,
			Object:     key,
			PlanID:     planID,
			ScenarioID: scenarioID,
			Field:      "result",
			Detail:     "committed combination results are frozen",
		}
	}
	s.comboPayload[key] = payload
	return nil
}

func (s *StateTracker) ComboPayload(planID, scenarioID string) (any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := ComboID{PlanID: planID, ScenarioID: scenarioID}.Key()
	payload, ok := s.comboPayload[key]
	return payload, ok
}

func (s *StateTracker) Snapshot() StateSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot := StateSnapshot{
		Plans:  make(map[string]PlanState, len(s.plans)),
		Combos: make(map[string]ComboState, len(s.combos)),
	}
	for key, value := range s.plans {
		snapshot.Plans[key] = value
	}
	for key, value := range s.combos {
		snapshot.Combos[key] = value
	}
	return snapshot
}

func (s *StateTracker) PlanState(planID string) PlanState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.plans[planID]
}

func (s *StateTracker) ComboState(planID, scenarioID string) ComboState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.combos[ComboID{PlanID: planID, ScenarioID: scenarioID}.Key()]
}

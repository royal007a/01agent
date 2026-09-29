package retrieval

import "time"

type Decision struct {
	Stop        bool
	Reason      StopReason
	Hard        bool
	Recommended bool
}

func Evaluate(st State, now time.Time) Decision {
	b := st.Budget
	if b.MaxRounds > 0 && b.UsedRounds >= b.MaxRounds {
		return Decision{true, StopBudgetRounds, true, false}
	}
	if b.MaxToolCalls > 0 && b.UsedToolCalls >= b.MaxToolCalls {
		return Decision{true, StopBudgetCalls, true, false}
	}
	if b.MaxTokens > 0 && b.UsedTokens >= b.MaxTokens {
		return Decision{true, StopBudgetTokens, true, false}
	}
	if b.MaxDuration > 0 && !b.StartedAt.IsZero() && now.Sub(b.StartedAt) >= b.MaxDuration {
		return Decision{true, StopBudgetTime, true, false}
	}
	active := ""
	for _, g := range st.Goals {
		if g.Status == GoalActive {
			active = g.ID
			break
		}
	}
	activeIDs := map[string]bool{}
	for _, g := range st.Goals {
		if g.ID == active {
			for _, id := range g.GapIDs {
				activeIDs[id] = true
			}
		}
	}
	covered := true
	for _, g := range st.Gaps {
		if active != "" && len(activeIDs) > 0 && !activeIDs[g.ID] {
			continue
		}
		if g.Status == GapOpen {
			covered = false
			break
		}
	}
	if active != "" && covered {
		return Decision{true, StopGoalsCovered, false, true}
	}
	if len(st.Rounds) > 0 && st.Rounds[len(st.Rounds)-1].DupCount > 0 {
		r := st.Rounds[len(st.Rounds)-1]
		if len(r.HitIDs) > 0 && float64(r.DupCount)/float64(len(r.HitIDs)) >= 0.8 {
			return Decision{true, StopHighDuplication, false, true}
		}
	}
	if len(st.Rounds) > 0 && len(st.Rounds[len(st.Rounds)-1].NewEvidence) == 0 {
		// First empty round is a recommendation: the model may switch strategy
		// or retriever once. A second consecutive empty round is a hard stop.
		if len(st.Rounds) > 1 && len(st.Rounds[len(st.Rounds)-2].NewEvidence) == 0 {
			return Decision{true, StopNoNewInfo, true, false}
		}
		return Decision{true, StopNoNewInfo, false, true}
	}
	return Decision{}
}

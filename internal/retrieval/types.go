package retrieval

import "time"

type Strategy string

const (
	StrategyEntity     Strategy = "new_entity"
	StrategyConstraint Strategy = "constraint"
	StrategyTerm       Strategy = "new_term"
	StrategyVerify     Strategy = "verify"
	StrategyCounter    Strategy = "counter"
)

type GoalStatus string

const (
	GoalActive   GoalStatus = "active"
	GoalDeferred GoalStatus = "deferred"
	GoalDone     GoalStatus = "done"
)

type GapStatus string

const (
	GapOpen         GapStatus = "open"
	GapCovered      GapStatus = "covered"
	GapUnanswerable GapStatus = "unanswerable"
	GapNeedsUser    GapStatus = "needs_user"
)

type StopReason string

const (
	StopNone              StopReason = ""
	StopBudgetRounds      StopReason = "budget_rounds"
	StopBudgetCalls       StopReason = "budget_tool_calls"
	StopBudgetTime        StopReason = "budget_time"
	StopBudgetTokens      StopReason = "budget_tokens"
	StopSourcesExhausted  StopReason = "sources_exhausted"
	StopGoalsCovered      StopReason = "gaps_covered"
	StopSufficient        StopReason = "sufficient"
	StopNoNewInfo         StopReason = "no_new_info"
	StopHighDuplication   StopReason = "high_duplication"
	StopMinimalSufficient StopReason = "minimal_sufficient"
	StopClarification     StopReason = "needs_clarification"
)

type Goal struct {
	ID, Question     string
	IndependentValue bool
	Status           GoalStatus
	GapIDs           []string
}
type Gap struct {
	ID, Question string
	Status       GapStatus
	CoveredBy    []string
}
type Chunk struct {
	ID, ParentID, Source, Path, Version, Text, Kind, StructPath, ContentHash string
	StartLine, EndLine, StartOffset, EndOffset                               int
	Trust                                                                    float64
}
type Evidence struct {
	ID, ChunkID, ParentID, Snippet string
	ChildIDs                       []string
	Round                          int
	GapIDs                         []string
	Score, Novelty                 float64
	Status                         string // candidate, accepted, superseded
}
type Round struct {
	N                                                   int
	Query, QueryFP                                      string
	Strategy                                            Strategy
	GoalID, GapID                                       string
	Sources, Retrievers, HitIDs, NewEvidence, Conflicts []string
	DupCount                                            int
	Decision                                            string
	StopReason                                          StopReason
	StartedAt                                           time.Time
	DurationMs                                          int64
}
type Budget struct {
	MaxRounds, MaxToolCalls, MaxTokens    int
	MaxDuration                           time.Duration
	UsedRounds, UsedToolCalls, UsedTokens int
	StartedAt                             time.Time
}
type State struct {
	Question string
	Goals    []Goal
	Gaps     []Gap
	Evidence map[string]Evidence
	Facts    []string
	Rounds   []Round
	Budget   Budget
	Status   string
}

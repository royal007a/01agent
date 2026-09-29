package tools

import (
	"context"
	"encoding/json"
	"fmt"
	ctxpkg "github.com/weberzhao/go-tiny-claw/internal/context"
	"github.com/weberzhao/go-tiny-claw/internal/retrieval"
	"github.com/weberzhao/go-tiny-claw/internal/runctx"
	"github.com/weberzhao/go-tiny-claw/internal/schema"
	"strings"
	"time"
)

type SearchTool struct{ Store *retrieval.LedgerStore }

func (t *SearchTool) Name() string   { return "search" }
func (t *SearchTool) ReadOnly() bool { return false }
func (t *SearchTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{Name: t.Name(), Description: "在工作区中检索并记录增量证据", InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"query": map[string]interface{}{"type": "string"}, "gap_id": map[string]interface{}{"type": "string"}, "strategy": map[string]interface{}{"type": "string"}}, "required": []string{"query", "gap_id", "strategy"}}}
}
func (t *SearchTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Query    string `json:"query"`
		GapID    string `json:"gap_id"`
		Strategy string `json:"strategy"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", err
	}
	s, _ := runctx.Session(ctx).(*ctxpkg.Session)
	task := runctx.RetrievalTask(ctx)
	if s == nil || task == "" {
		return "", fmt.Errorf("retrieval requires session and task")
	}
	if strings.TrimSpace(in.Query) == "" || strings.TrimSpace(in.GapID) == "" {
		return "", fmt.Errorf("query and gap_id are required")
	}
	allowed := map[string]bool{"new_entity": true, "constraint": true, "new_term": true, "verify": true, "counter": true}
	if !allowed[in.Strategy] {
		return "", fmt.Errorf("invalid strategy")
	}
	rs := (&retrieval.Searcher{WorkDir: s.WorkDir, MaxFiles: 1000}).Search(in.Query, 8)
	var out strings.Builder
	count := 0
	stopNote := ""
	err := t.Store.Update(s.ID, task, func(st *retrieval.State) error {
		if st.Budget.MaxRounds == 0 {
			st.Budget.MaxRounds = 3
		}
		st.Budget.UsedRounds = len(st.Rounds)
		if d := retrieval.Evaluate(*st, time.Now()); d.Stop && d.Hard {
			return fmt.Errorf("retrieval stopped: %s", d.Reason)
		}
		found := false
		for _, g := range st.Gaps {
			if g.ID == in.GapID {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("unknown gap %s", in.GapID)
		}
		fp := retrieval.QueryFingerprint(in.Query, nil, nil)
		for _, r := range st.Rounds {
			if r.QueryFP == fp {
				return fmt.Errorf("duplicate query")
			}
		}
		round := retrieval.Round{N: len(st.Rounds) + 1, Query: in.Query, QueryFP: fp, Strategy: retrieval.Strategy(in.Strategy), GapID: in.GapID, Decision: "continue", Retrievers: []string{"bm25", "rg"}}
		// One evidence item per parent. Build the display first, then persist only
		// evidence that actually fits the 6KB tool-result budget.
		seenParent := map[string]bool{}
		for _, r := range rs {
			pid := r.Chunk.ParentID
			if pid == "" {
				pid = r.Chunk.ID
			}
			if seenParent[pid] {
				continue
			}
			text := r.Chunk.Text
			path := r.Chunk.Path
			sl, el := r.Chunk.StartLine, r.Chunk.EndLine
			if r.Parent != nil && len(r.Parent.Text) <= 6000 {
				text = r.Parent.Text
				path = r.Parent.Path
				sl, el = r.Parent.StartLine, r.Parent.EndLine
			}
			line := fmt.Sprintf("[%s:%d-%d parent_id=%s evidence_id=%s] %s\n", path, sl, el, pid, r.Chunk.ID, text)
			if out.Len()+len(line) > 6000 && r.Parent != nil && text == r.Parent.Text {
				text, path, sl, el = r.Chunk.Text, r.Chunk.Path, r.Chunk.StartLine, r.Chunk.EndLine
				line = fmt.Sprintf("[%s:%d-%d parent_id=%s evidence_id=%s] %s\n", path, sl, el, pid, r.Chunk.ID, text)
			}
			if out.Len()+len(line) > 6000 {
				continue
			}
			short := r.Chunk.ID
			if len(short) > 12 {
				short = short[:12]
			}
			eid := fmt.Sprintf("%s-r%d", short, round.N)
			duplicate := false
			for _, ev := range st.Evidence {
				if ev.ChunkID == r.Chunk.ID {
					for _, gid := range ev.GapIDs {
						if gid == in.GapID {
							duplicate = true
						}
					}
					if duplicate {
						break
					}
				}
			}
			if duplicate {
				round.DupCount++
				seenParent[pid] = true
				continue
			}
			st.Evidence[eid] = retrieval.Evidence{ID: eid, ChunkID: r.Chunk.ID, ParentID: pid, ChildIDs: []string{r.Chunk.ID}, Snippet: text, Round: round.N, GapIDs: []string{in.GapID}, Score: r.Score, Novelty: 1, Status: "candidate"}
			round.NewEvidence = append(round.NewEvidence, eid)
			round.HitIDs = append(round.HitIDs, r.Chunk.ID)
			out.WriteString(strings.Replace(line, "evidence_id="+r.Chunk.ID, "evidence_id="+eid, 1))
			count++
			seenParent[pid] = true
		}
		st.Rounds = append(st.Rounds, round)
		st.Budget.UsedRounds = len(st.Rounds)
		if d := retrieval.Evaluate(*st, time.Now()); d.Stop {
			stopNote = fmt.Sprintf("stop_recommended=%t stopped=%t reason=%s\n", d.Recommended, d.Hard, d.Reason)
		}
		// Search finds candidates; only evidence.mark_covered closes a gap.
		return nil
	})
	if err != nil {
		return "", err
	}
	if count == 0 {
		return stopNote + "无新增证据", nil
	}
	return stopNote + out.String(), nil
}

type EvidenceTool struct{ Store *retrieval.LedgerStore }

func (t *EvidenceTool) Name() string   { return "evidence" }
func (t *EvidenceTool) ReadOnly() bool { return false }
func (t *EvidenceTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{Name: t.Name(), Description: "管理检索目标、缺口和事实账本", InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"action": map[string]interface{}{"type": "string"}, "id": map[string]interface{}{"type": "string"}, "text": map[string]interface{}{"type": "string"}, "evidence_ids": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}}}, "required": []string{"action"}}}
}
func (t *EvidenceTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Action      string   `json:"action"`
		ID          string   `json:"id"`
		Text        string   `json:"text"`
		EvidenceIDs []string `json:"evidence_ids"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", err
	}
	s, _ := runctx.Session(ctx).(*ctxpkg.Session)
	task := runctx.RetrievalTask(ctx)
	if s == nil || task == "" {
		return "", fmt.Errorf("retrieval requires session and task")
	}
	var st retrieval.State
	err := t.Store.Update(s.ID, task, func(x *retrieval.State) error {
		st = *x
		switch in.Action {
		case "add_gap":
			if strings.TrimSpace(in.ID) == "" {
				return fmt.Errorf("gap id required")
			}
			for _, g := range x.Gaps {
				if g.ID == in.ID {
					return fmt.Errorf("duplicate gap")
				}
			}
			x.Gaps = append(x.Gaps, retrieval.Gap{ID: in.ID, Question: in.Text, Status: retrieval.GapOpen})
			st = *x
		case "mark_fact":
			x.Facts = append(x.Facts, in.Text)
			st = *x
		case "mark_covered":
			if len(in.EvidenceIDs) == 0 {
				return fmt.Errorf("mark_covered requires evidence_ids")
			}
			for _, eid := range in.EvidenceIDs {
				ev, ok := x.Evidence[eid]
				if !ok {
					return fmt.Errorf("unknown evidence %s", eid)
				}
				valid := false
				for _, gid := range ev.GapIDs {
					if gid == in.ID {
						valid = true
					}
				}
				if !valid {
					return fmt.Errorf("evidence %s does not belong to gap", eid)
				}
			}
			for i := range x.Gaps {
				if x.Gaps[i].ID == in.ID {
					x.Gaps[i].Status = retrieval.GapCovered
					x.Gaps[i].CoveredBy = append(x.Gaps[i].CoveredBy, in.EvidenceIDs...)
					st = *x
					return nil
				}
			}
			return fmt.Errorf("unknown gap")
		case "mark_unanswerable":
			for i := range x.Gaps {
				if x.Gaps[i].ID == in.ID {
					x.Gaps[i].Status = retrieval.GapUnanswerable
					st = *x
					return nil
				}
			}
			return fmt.Errorf("unknown gap")
		case "finish":
			for _, g := range x.Gaps {
				if g.Status == retrieval.GapOpen {
					return fmt.Errorf("gap %s is not covered", g.ID)
				}
			}
			x.Status = "done"
		default:
			return fmt.Errorf("unknown evidence action")
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("evidence updated: %s (%d gaps, %d rounds)", in.Action, len(st.Gaps), len(st.Rounds)), nil
}

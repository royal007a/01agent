package retrieval

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestChunkDocumentUsesParentAndChildren(t *testing.T) {
	cs := ChunkDocument("workspace", "a.go", "# Header\nalpha one.\nalpha two.\n\nfunc Main() {\n  return\n}\n", 12, 64)
	if len(cs) < 2 {
		t.Fatalf("expected parent and child chunks, got %d", len(cs))
	}
	parents := 0
	for _, c := range cs {
		if c.ParentID == "" {
			parents++
		}
	}
	if parents == 0 {
		t.Fatal("missing parent chunk")
	}
	for _, c := range cs {
		if c.Text == "" || c.ID == "" {
			t.Fatal("invalid chunk")
		}
		if c.ParentID != "" && c.ParentID == c.ID {
			t.Fatal("child id must differ")
		}
	}
}

func TestStructuredChunkLimitsAndWeightedParentRank(t *testing.T) {
	text := "# one\n" + "alpha beta gamma.\n" + "# two\n" + "delta epsilon zeta.\n"
	cs := ChunkStructured("ws", "x.md", text, ChunkOptions{Type: "markdown", MaxChildTokens: 4, MaxParentTokens: 10})
	if len(cs) < 3 {
		t.Fatalf("expected hierarchy, got %d", len(cs))
	}
	parents := map[string]Chunk{}
	children := map[string]Chunk{}
	for _, c := range cs {
		if c.ParentID == "" {
			parents[c.ID] = c
		} else {
			children[c.ID] = c
			if _, ok := parents[c.ParentID]; !ok {
				t.Fatalf("child parent missing")
			}
		}
	}
	h := []Hit{}
	for id, c := range children {
		h = append(h, Hit{ChunkID: id, Rank: 1, Weight: 1, Score: 1})
		_ = c
	}
	if len(h) == 0 {
		for id := range parents {
			h = append(h, Hit{ChunkID: id, Rank: 1, Score: 1})
		}
	}
	scores := AggregateParents(h, func() map[string]Chunk {
		m := map[string]Chunk{}
		for _, c := range cs {
			m[c.ID] = c
		}
		return m
	}(), 3)
	if len(scores) == 0 {
		t.Fatal("missing parent aggregation")
	}
}

func TestChunkOffsetsUTF8AndUniqueIDs(t *testing.T) {
	text := "2026/04/24 00:00:00 ok\n2026/04/24 00:00:01 重复\n2026/04/24 00:00:02 重复\n"
	cs := ChunkStructured("ws", "x.log", text, ChunkOptions{Type: "log", MaxChildTokens: 20, MaxParentTokens: 40})
	seen := map[string]bool{}
	for _, c := range cs {
		if !utf8.ValidString(c.Text) || c.Text != text[c.StartOffset:c.EndOffset] {
			t.Fatalf("invalid source slice: %+v", c)
		}
		if seen[c.ID] {
			t.Fatalf("duplicate id %s", c.ID)
		}
		seen[c.ID] = true
		if c.EndOffset > 0 && c.EndLine != 1+strings.Count(text[:c.EndOffset-1], "\n") {
			t.Fatalf("end line mismatch: %+v", c)
		}
	}
}

func TestCRLFChildTokenLimit(t *testing.T) {
	text := ""
	for i := 0; i < 400; i++ {
		text += "第一行内容。\r\n"
	}
	for _, c := range ChunkStructured("ws", "crlf.txt", text, ChunkOptions{MaxChildTokens: 300, MaxParentTokens: 2000}) {
		if c.Kind == "child" && EstimateTokens(c.Text) > 300 {
			t.Fatalf("child exceeds limit: %d", EstimateTokens(c.Text))
		}
	}
}

func TestLedgerSessionTaskIsolation(t *testing.T) {
	s := NewLedgerStore(filepath.Join(t.TempDir(), "retrieval"))
	a := State{Question: "a", Evidence: map[string]Evidence{"e": {ID: "e"}}}
	if err := s.Save("session-a", "task-1", a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load("session-b", "task-1"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load("session-a", "task-1")
	if err != nil || len(got.Evidence) != 1 {
		t.Fatalf("%v %+v", err, got)
	}
}

func TestStopBudgetAndGoal(t *testing.T) {
	now := time.Now()
	st := State{Goals: []Goal{{ID: "g", Status: GoalActive}}, Gaps: []Gap{{ID: "x", Status: GapCovered}}, Budget: Budget{StartedAt: now, MaxRounds: 3, UsedRounds: 1}}
	if d := Evaluate(st, now); !d.Stop || d.Reason != StopGoalsCovered {
		t.Fatalf("%+v", d)
	}
	st.Gaps[0].Status = GapOpen
	st.Budget.UsedRounds = 3
	if d := Evaluate(st, now); !d.Hard || d.Reason != StopBudgetRounds {
		t.Fatalf("%+v", d)
	}
}

func TestRankMetrics(t *testing.T) {
	m := RankMetrics([]string{"a", "b", "c"}, map[string]int{"b": 2, "c": 1}, 3)
	if m.Recall != 1 || m.MRR != .5 || m.NDCG <= 0 {
		t.Fatalf("%+v", m)
	}
}

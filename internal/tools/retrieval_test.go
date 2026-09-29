package tools

import (
	"context"
	"encoding/json"
	ctxpkg "github.com/weberzhao/go-tiny-claw/internal/context"
	"github.com/weberzhao/go-tiny-claw/internal/retrieval"
	"github.com/weberzhao/go-tiny-claw/internal/runctx"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSearchSchemaAndEvidencePersist(t *testing.T) {
	wd := t.TempDir()
	_ = os.WriteFile(filepath.Join(wd, "a.md"), []byte("# A\nhello retrieval evidence"), 0644)
	st := retrieval.NewLedgerStore(filepath.Join(t.TempDir(), "ledger"))
	ctx := runctx.WithSession(context.Background(), ctxpkg.NewSession("s", wd))
	ctx = runctx.WithRetrievalTask(ctx, "task")
	e := &EvidenceTool{Store: st}
	b, _ := json.Marshal(map[string]string{"action": "add_gap", "id": "g", "text": "find"})
	if _, err := e.Execute(ctx, b); err != nil {
		t.Fatal(err)
	}
	q := &SearchTool{Store: st}
	b, _ = json.Marshal(map[string]string{"query": "hello", "gap_id": "g", "strategy": "new_term"})
	out, err := q.Execute(ctx, b)
	if err != nil || !strings.Contains(out, "a.md") {
		t.Fatalf("%v %s", err, out)
	}
	x0, _ := st.Load("s", "task")
	var eid string
	for id := range x0.Evidence {
		eid = id
		break
	}
	b, _ = json.Marshal(map[string]interface{}{"action": "mark_covered", "id": "g", "evidence_ids": []string{eid}})
	if _, err := e.Execute(ctx, b); err != nil {
		t.Fatal(err)
	}
	x, err := st.Load("s", "task")
	if err != nil || len(x.Rounds) != 1 || x.Gaps[0].Status != "covered" {
		t.Fatalf("%v %+v", err, x)
	}
	b, _ = json.Marshal(map[string]string{"action": "finish"})
	if _, err := e.Execute(ctx, b); err != nil {
		t.Fatal(err)
	}
	x, err = st.Load("s", "task")
	if err != nil || x.Status != "done" {
		t.Fatalf("finish not persisted: %v %+v", err, x)
	}
}

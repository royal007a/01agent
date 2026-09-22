package workitem

import (
	"context"
	"path/filepath"
	"testing"
)

func TestControlPlaneEvaluationGate(t *testing.T) {
	suite, err := LoadEvaluationSuite(filepath.Join("..", "..", "evals", "control-plane.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(suite.Cases); got != 15 {
		t.Fatalf("case count = %d, want 15", got)
	}
	report, err := RunEvaluation(context.Background(), suite)
	if err != nil {
		t.Fatal(err)
	}
	if !report.GatePassed || report.Passed != report.Cases {
		t.Fatalf("gate failed: %+v", report)
	}
}

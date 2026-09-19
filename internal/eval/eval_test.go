package eval

import (
	"context"
	"path/filepath"
	"testing"
)

func TestRuntimeSuitePassesGate(t *testing.T) {
	suite, err := LoadSuite(filepath.Join("..", "..", "evals", "runtime.json"))
	if err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), suite, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !report.GatePassed || report.Passed != len(suite.Cases) {
		t.Fatalf("report = %#v", report)
	}
	if report.ToolAccuracy != 1 || report.ToolCorrect != len(suite.Cases) {
		t.Fatalf("tool metrics = %d/%d", report.ToolCorrect, len(suite.Cases))
	}
	if report.TotalTurns == 0 || report.AverageTurns == 0 || report.TotalLatencyMS == 0 || report.AverageLatencyMS == 0 {
		t.Fatalf("runtime metrics missing: %#v", report)
	}
	if len(report.TerminationReasons) < 5 {
		t.Fatalf("terminal reason histogram is incomplete: %#v", report.TerminationReasons)
	}
}

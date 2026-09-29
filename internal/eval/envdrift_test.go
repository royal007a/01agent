package eval

import (
	"strings"
	"testing"

	"github.com/weberzhao/go-tiny-claw/internal/envlock"
)

func TestReportFlagsEnvDriftAgainstBaseline(t *testing.T) {
	env := func(observed string) *envlock.Lock {
		return &envlock.Lock{Runtime: envlock.Runtime{OS: "darwin", Arch: "arm64", Go: "go1.26.4"}, Model: envlock.Model{Requested: "deepseek-v4-flash", Observed: observed}}
	}
	base := Report{Env: env("deepseek-v4-flash-0801"), Results: []Result{{ID: "t01", Passed: true}}}
	cur := Report{Env: env("deepseek-v4.1-flash"), Results: []Result{
		{ID: "t01", ObservedModel: "deepseek-v4.1-flash"},
		{ID: "t02", ObservedModel: "deepseek-v4.2-flash"},
	}}
	md := cur.Markdown(&base)
	for _, want := range []string{"环境与基线不一致", "model observed", "多个模型版本"} {
		if !strings.Contains(md, want) {
			t.Errorf("报告缺少 %q:\n%s", want, md)
		}
	}
	// 旧基线没有环境快照：视为未测，不报环境差异
	old := Report{Results: base.Results}
	if ds := cur.EnvDrift(&old); ds != nil || strings.Contains(cur.Markdown(&old), "环境与基线不一致") {
		t.Errorf("旧报告缺快照不应报环境差异: %v", ds)
	}
}

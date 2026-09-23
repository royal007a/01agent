package engine

import (
	"strings"
	"testing"

	"github.com/weberzhao/go-tiny-claw/internal/schema"
)

func fail(id string) schema.Message { return schema.Message{ToolCallID: id, IsError: true} }
func ok(id string) schema.Message   { return schema.Message{ToolCallID: id} }

func call(name, args string) schema.ToolCall {
	return schema.ToolCall{ID: "x", Name: name, Arguments: []byte(args)}
}

func TestFingerprintNormalization(t *testing.T) {
	a := Fingerprint("read_file", []byte(`{"path": "/tmp/a.txt"}`))
	for _, v := range []string{`{"path":"/tmp/a.txt "}`, `{"path":"./../tmp/a.txt"}`, `{ "path" : "tmp//a.txt" }`} {
		if Fingerprint("read_file", []byte(v)) != a {
			t.Fatalf("%s 应与 /tmp/a.txt 视为同一动作", v)
		}
	}
	if Fingerprint("read_file", []byte(`{"path":"b.txt"}`)) == a || Fingerprint("bash", []byte(`{"path":"/tmp/a.txt"}`)) == a {
		t.Fatal("不同动作不应碰撞")
	}
	if Fingerprint("bash", []byte(`{"command":"go   build ./..."}`)) != Fingerprint("bash", []byte(`{"command":"go build ./..."}`)) {
		t.Fatal("多余空白应归一")
	}
}

func TestReminderSlidingWindowPerSession(t *testing.T) {
	r := NewReminderInjector()
	bad := call("read_file", `{"path":"secret.txt"}`)
	ls := call("bash", `{"command":"ls"}`)
	// 失败-成功(ls)-失败-失败：课程实现会被中间的成功清零，这里仍应触发
	if r.CheckAndInject("s1", []schema.ToolCall{bad}, []schema.Message{fail("x")}) != nil {
		t.Fatal("1 次不应触发")
	}
	r.CheckAndInject("s1", []schema.ToolCall{ls}, []schema.Message{ok("x")})
	// 另一个会话的失败不应计入 s1
	r.CheckAndInject("s2", []schema.ToolCall{bad}, []schema.Message{fail("x")})
	if r.CheckAndInject("s1", []schema.ToolCall{call("read_file", `{"path":"./secret.txt "}`)}, []schema.Message{fail("x")}) != nil {
		t.Fatal("2 次不应触发")
	}
	m := r.CheckAndInject("s1", []schema.ToolCall{ls, bad}, []schema.Message{ok("a"), fail("b")})
	if m == nil || m.Role != schema.RoleUser || !strings.Contains(m.Content, "SYSTEM REMINDER") || !strings.Contains(m.Content, "read_file") {
		t.Fatalf("第 3 次失败应触发干预: %#v", m)
	}
	if r.CheckAndInject("s1", []schema.ToolCall{bad}, []schema.Message{fail("x")}) != nil {
		t.Fatal("触发后应重新计数，避免每轮轰炸")
	}
}

package engine

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"path"
	"strings"
	"sync"

	"github.com/weberzhao/go-tiny-claw/internal/schema"
)

// DoomLoopThreshold 同一"本质动作"在滑动窗口内失败多少次触发干预（第 15 讲）。
const DoomLoopThreshold = 3

// reminderWindow 每个会话保留最近多少次工具结果用于判断。
const reminderWindow = 12

// ReminderInjector 在运行时监控工具调用轨迹，在模型陷入执念时注入强力打断信息。
//
// 相对课程实现的改进：
//   - 状态按 Session 隔离（课程挂在引擎上，多个飞书会话会互相污染计数）；
//   - 一批并行调用全部纳入分析（课程只看第 0 个）；
//   - 课程"任何一次成功就清空所有计数"，模型只要穿插一次 ls 就能永远逃过检测，
//     这里改为滑动窗口统计；
//   - 指纹做参数规范化（第 15 讲思考题）：键排序、去首尾空白、路径 Clean，
//     "/tmp/a.txt " 与 "./../tmp/a.txt" 这类微小变体会被识别为同一动作。
type ReminderInjector struct {
	mu     sync.Mutex
	window map[string][]outcome // sessionID -> 最近的工具结果
}

type outcome struct {
	fp     string
	failed bool
}

func NewReminderInjector() *ReminderInjector {
	return &ReminderInjector{window: make(map[string][]outcome)}
}

// Fingerprint 生成工具调用的规范化指纹。
func Fingerprint(toolName string, args []byte) string {
	var v any
	norm := strings.TrimSpace(string(args))
	if err := json.Unmarshal(args, &v); err == nil {
		b, _ := json.Marshal(normalize(v)) // encoding/json 对 map 键天然排序
		norm = string(b)
	}
	h := md5.Sum([]byte(toolName + "\x00" + norm))
	return hex.EncodeToString(h[:])
}

func normalize(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			x[k] = normalize(val)
			if s, ok := x[k].(string); ok && strings.Contains(strings.ToLower(k), "path") {
				x[k] = cleanPath(s)
			}
		}
		return x
	case []any:
		for i := range x {
			x[i] = normalize(x[i])
		}
		return x
	case string:
		return strings.Join(strings.Fields(x), " ")
	default:
		return v
	}
}

func cleanPath(p string) string {
	p = path.Clean(p)
	for strings.HasPrefix(p, "../") {
		p = p[3:]
	}
	return strings.TrimPrefix(p, "/")
}

// CheckAndInject 分析本轮所有工具的执行结果，必要时返回一条要追加到 Session 末尾的 User 提醒。
func (r *ReminderInjector) CheckAndInject(sessionID string, calls []schema.ToolCall, results []schema.Message) *schema.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	w := r.window[sessionID]
	for i, c := range calls {
		failed := i < len(results) && results[i].IsError
		w = append(w, outcome{fp: Fingerprint(c.Name, c.Arguments), failed: failed})
	}
	if len(w) > reminderWindow {
		w = w[len(w)-reminderWindow:]
	}

	var trigger *schema.ToolCall
	var count int
	for i, c := range calls {
		if i >= len(results) || !results[i].IsError {
			continue
		}
		fp := Fingerprint(c.Name, c.Arguments)
		n := 0
		for _, o := range w {
			if o.fp == fp && o.failed {
				n++
			}
		}
		log.Printf("[Reminder] 监控到工具 %s 执行失败，该动作特征在最近窗口内失败次数: %d", c.Name, n)
		if n >= DoomLoopThreshold {
			trigger, count = &calls[i], n
			// 触发后清掉该指纹的历史，避免每一轮都重复轰炸
			kept := w[:0]
			for _, o := range w {
				if o.fp != fp {
					kept = append(kept, o)
				}
			}
			w = kept
			break
		}
	}
	r.window[sessionID] = w
	if trigger == nil {
		return nil
	}
	log.Println("[Reminder] ⚠️ 触发死循环干预！注入强力修正指令。")
	return &schema.Message{
		Role: schema.RoleUser, // 必须是 User：在下一次请求中拥有最高的近因效应权重
		Content: fmt.Sprintf(`[SYSTEM REMINDER 警告]
你似乎陷入了死循环。你已经 %d 次以本质相同的参数调用 '%s' 工具，并且都失败了。
请立即停止这种无效的重试！你的注意力被当前的报错过度吸引了。
你需要：
1. 停止猜测参数，跳出当前的局部思维。
2. 彻底改变你的策略（例如先用 bash 的 ls/find 核实环境事实）。
3. 如果你确实无法通过系统工具解决当前问题，请直接结束任务并向用户说明你需要什么人工帮助，而不是继续盲目消耗 API 资源尝试。`, count, trigger.Name),
	}
}

// Forget 清除某个会话的监控状态。
func (r *ReminderInjector) Forget(sessionID string) {
	r.mu.Lock()
	delete(r.window, sessionID)
	r.mu.Unlock()
}

package context

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/weberzhao/go-tiny-claw/internal/knowledge"
	"github.com/weberzhao/go-tiny-claw/internal/schema"
)

// minimalCore 极简内核：只硬编码身份与最底线的纪律（第 10 讲）。
const minimalCore = `# 核心身份
你名叫 go-tiny-claw，一个由驾驭工程驱动的骨灰级研发助手。
你具备极简主义哲学，拒绝废话。你能通过系统提供的内置工具，创建、读取、修改和执行工作区中的代码。

# 核心纪律 (CRITICAL)
1. 如需检查文件是否存在，请使用 bash 的 ls 或 test -f，而不是对目录使用 read_file。
2. 创建新文件时，务必使用 write_file，并同时提供 path 和 content 参数。
3. 编辑文件前务必先读取现有文件，以理解上下文。
4. 无论何时你需要写代码或创建文件，都要直接使用 write_file 工具。
5. 遇到工具执行报错时，仔细阅读 stderr，尝试自己修正命令并重试。
6. 始终用中文回复，以便传达你的进展和想法。
`

// planModeRules Plan Mode 的状态外部化强制规范（第 13 讲）。
const planModeRules = "\n# 长程任务与状态外部化强制规范 (Plan Mode: ON)\n" +
	"!!! 警告：本模式下，你绝对不能依赖自己的短期记忆。你必须将所有的架构思路和执行进度持久化到物理文件中。 !!!\n" +
	"当你收到一条新指令被唤醒时，你必须、且只能按照以下【绝对顺序】执行你的动作：\n\n" +
	"**[STEP 1: 强制环境嗅探 (Bootstrapping)]**\n" +
	"- 收到指令后，你必须第一时间使用 bash (如: `ls -la`) 检查当前工作区根目录下是否已经存在 `PLAN.md` 和 `TODO.md`。\n" +
	"- **分支 A (全新任务)**：如果这两个文件不存在，说明这是一个全新的任务。你必须使用 write_file 依次创建它们：\n" +
	"  1. 先创建 `PLAN.md`，写下你的理解、架构设计、技术选型。\n" +
	"  2. 再创建 `TODO.md`，拆解出具体的可执行步骤（使用标准的 Markdown Checkbox 格式，如 `- [ ] 步骤1`）。\n" +
	"- **分支 B (断点续传/任务唤醒)**：如果这两个文件已经存在，**绝对不要覆盖它们！** 这意味着系统刚刚重启，或者人类接管了进度。" +
	"你必须立即使用 read_file 仔细阅读 `PLAN.md` 了解全局目标，并阅读 `TODO.md` 寻找第一个未被打勾的 `- [ ]` 任务，从那里直接继续干活。\n\n" +
	"**[STEP 2: 严格的单步执行与实时打勾]**\n" +
	"- 开始执行 `TODO.md` 中未完成的任务。\n" +
	"- **强制约束**：每当你通过 write_file 或 bash 真正完成了一个子任务后，你**必须立即停下来**，优先使用 edit_file 工具，将 `TODO.md` 中对应的行修改为 `- [x]`。\n" +
	"- 绝对不允许\"一口气写完所有代码最后再打勾\"。做完一步，必须打勾一步！\n\n" +
	"**[STEP 3: 迷失时的自救]**\n" +
	"- 如果你在执行中遇到了报错，或者不知道下一步该干嘛了，立即使用 read_file 重新读取 `TODO.md` 确认自己的位置。\n"

// PromptComposer 根据工作区环境动态生成 System Prompt：内核 + Plan Mode + AGENTS.md + Skills。
type PromptComposer struct {
	workDir     string
	planMode    bool
	skillLoader *SkillLoader
}

func NewPromptComposer(workDir string, planMode bool) *PromptComposer {
	return &PromptComposer{workDir: workDir, planMode: planMode, skillLoader: NewSkillLoader(workDir)}
}

// Build 组装并返回一条完整的 RoleSystem 消息。
func (c *PromptComposer) Build() schema.Message {
	var b strings.Builder
	// 1. 极简内核
	b.WriteString(minimalCore)
	// 1.5 运行环境（课程之外的增强，参考 Claude Code 的 env 注入）：告诉模型"你在哪、这里有什么"。
	// 实测本地 8B 模型在不知道工作区的情况下会去猜 /usr/local/nginx/... 这类系统路径，一路撞墙。
	b.WriteString(envSection(c.workDir))
	// 2. Plan Mode：只有显式开启时才注入重型的状态外部化规范
	if c.planMode {
		b.WriteString(planModeRules)
	}
	// 3. 外部化状态：项目专属规范 AGENTS.md（由人类维护）
	if content, err := os.ReadFile(filepath.Join(c.workDir, "AGENTS.md")); err == nil {
		b.WriteString("\n# 项目专属指南 (来自 AGENTS.md)\n")
		b.WriteString("以下是当前工作区特有的架构规范与注意事项，你的行为必须绝对符合以下要求：\n```markdown\n")
		b.WriteString(strings.TrimSpace(string(content)))
		b.WriteString("\n```\n")
	}
	// 3.5 跨会话沉淀的知识（动态知识更新的闭环：规则、决策、偏好、发现）
	b.WriteString(knowledge.RenderPrompt(c.workDir))
	// 4. 技能外挂
	b.WriteString(c.skillLoader.LoadAll())
	return schema.Message{Role: schema.RoleSystem, Content: b.String()}
}

// envSection 生成运行环境描述：工作区绝对路径、系统、日期与顶层目录清单（最多 40 项）。
func envSection(workDir string) string {
	if workDir == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n# 运行环境\n")
	fmt.Fprintf(&b, "- 工作区（你的当前目录，bash 在这里执行，文件工具的相对路径以它为根）：%s\n", workDir)
	fmt.Fprintf(&b, "- 系统：%s/%s，日期：%s\n", runtime.GOOS, runtime.GOARCH, time.Now().Format("2006-01-02"))
	entries, err := os.ReadDir(workDir)
	if err != nil {
		return b.String()
	}
	var names []string
	for _, e := range entries {
		n := e.Name()
		if n == ".claw" || n == ".git" {
			continue
		}
		if e.IsDir() {
			n += "/"
		}
		names = append(names, n)
	}
	if len(names) == 0 {
		b.WriteString("- 工作区目前是空的\n")
		return b.String()
	}
	more := ""
	if len(names) > 40 {
		more = fmt.Sprintf(" …（共 %d 项）", len(names))
		names = names[:40]
	}
	fmt.Fprintf(&b, "- 工作区顶层内容：%s%s\n", strings.Join(names, "  "), more)
	b.WriteString("- 任务涉及的文件优先在工作区内查找，不要臆测系统路径。\n")
	return b.String()
}

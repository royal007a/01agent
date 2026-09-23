package context

import (
	"fmt"
	"strings"
)

// RecoveryManager 在工具执行失败时，按工具与报错特征注入"锦囊妙计"（第 14 讲）。
//
// 匹配依据两类相对稳定的信号：POSIX 标准错误文案（no such file or directory 等），
// 以及本项目工具内部固定的报错常量（见 tools/edit_file.go、tools/path.go）。
type RecoveryManager struct{}

func NewRecoveryManager() *RecoveryManager { return &RecoveryManager{} }

type recoveryRule struct {
	tools    []string // 为空表示对所有工具生效
	patterns []string // 任一命中即可（大小写不敏感）
	hint     string
}

var recoveryRules = []recoveryRule{
	{[]string{"edit_file"}, []string{"未找到 old_text", "找不到该代码片段"},
		"你提供的 old_text 与文件当前内容不一致，或者缺少必要的缩进。请先使用 `read_file` 工具重新读取该文件，获取最新、准确的内容后，再重新发起编辑。"},
	{[]string{"edit_file"}, []string{"匹配到了多处", "提供更多"},
		"你的 old_text 不够具体，命中了多个相同代码块。请在 old_text 中增加上下相邻的几行代码，以确保替换的唯一性。"},
	{nil, []string{"越出了工作区边界"},
		"文件工具只能访问工作区内的路径。请改用相对工作区的路径；如确需查看工作区外的内容，请先向用户说明原因。"},
	{nil, []string{"是一个目录"},
		"目标是目录而不是文件。请使用 `bash` 执行 `ls -la <目录>` 查看其中的文件，再读取具体文件。"},
	{[]string{"read_file", "write_file", "edit_file"}, []string{"no such file or directory"},
		"路径似乎不正确。请不要凭空猜测，先使用 `bash` 执行 `ls -la` 或 `find . -name '<文件名>'` 命令查找正确的目录结构和文件名。"},
	{nil, []string{"permission denied"},
		"你没有权限操作该文件。请检查工作区限制，或者思考是否需要修改其他文件。"},
	{[]string{"bash"}, []string{"command not found"},
		"系统中未安装该命令。请先思考：是否有替代命令？或者你需要先编写脚本进行安装？"},
	{[]string{"bash"}, []string{"超时", "deadlineexceeded"},
		"该命令执行被超时强杀。如果它是一个常驻服务（如 server 或 watch），请将其转入后台执行（例如 `nohup ./server > server.log 2>&1 &`），不要阻塞主线程。"},
	{[]string{"bash"}, []string{"syntax error"},
		"Bash 语法错误。请检查引号转义或特殊字符，确保命令在终端中可直接运行；复杂内容建议先用 write_file 写成脚本再执行。"},
	{nil, []string{"不存在名为"},
		"你调用了一个不存在的工具。请只使用系统提供的工具列表中的工具。"},
	{nil, []string{"参数解析失败"},
		"工具参数不是合法的 JSON 或字段类型不对。请严格按照工具的 JSON Schema 重新构造参数。"},
}

// AnalyzeAndInject 接收原始报错，匹配已知特征模式，返回增强后的报错信息；未命中则原样返回。
func (rm *RecoveryManager) AnalyzeAndInject(toolName string, rawError string) string {
	lower := strings.ToLower(rawError)
	for _, r := range recoveryRules {
		if len(r.tools) > 0 && !contains(r.tools, toolName) {
			continue
		}
		for _, p := range r.patterns {
			if strings.Contains(lower, strings.ToLower(p)) {
				return fmt.Sprintf("%s\n\n[系统救援指南]: %s", rawError, r.hint)
			}
		}
	}
	return rawError
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

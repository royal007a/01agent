// Package textutil 提供 UTF-8 安全的截断工具。
//
// 课程里的截断都是 s[:n] 按字节硬切，遇到中文会把一个字切成半个，
// 产生非法 UTF-8；部分模型 API 会因此报 400。这里统一做字符边界对齐。
package textutil

import "unicode/utf8"

// Head 返回 s 的前 n 个字节以内、落在字符边界上的最长前缀。
func Head(s string, n int) string {
	if n >= len(s) {
		return s
	}
	if n <= 0 {
		return ""
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// Tail 返回 s 的后 n 个字节以内、落在字符边界上的最长后缀。
func Tail(s string, n int) string {
	if n >= len(s) {
		return s
	}
	if n <= 0 {
		return ""
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return s[i:]
}

// Truncate 超过 max 字节时截断并追加 suffix。
func Truncate(s string, max int, suffix string) string {
	if len(s) <= max {
		return s
	}
	return Head(s, max) + suffix
}

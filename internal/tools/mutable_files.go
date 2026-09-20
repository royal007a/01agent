package tools

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/royal007a/01agent/internal/schema"
)

const maximumMutableFileBytes = 1 << 20

type WriteFileTool struct{ workspaceRoot }
type EditFileTool struct{ workspaceRoot }

type workspaceRoot struct {
	workDir string
	root    *os.Root
}

type writeFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type editFileArgs struct {
	Path           string `json:"path"`
	OldText        string `json:"old_text"`
	NewText        string `json:"new_text"`
	ReplaceAll     bool   `json:"replace_all,omitempty"`
	ExpectedSHA256 string `json:"expected_sha256,omitempty"`
}

var mutablePathLocks sync.Map
var errEditConflict = errors.New("mutable file changed")

type editMatch struct {
	start int
	end   int
	level string
}

func NewWriteFileTool(workDir string) (*WriteFileTool, error) {
	root, err := newWorkspaceRoot(workDir)
	if err != nil {
		return nil, err
	}
	return &WriteFileTool{workspaceRoot: root}, nil
}

func NewEditFileTool(workDir string) (*EditFileTool, error) {
	root, err := newWorkspaceRoot(workDir)
	if err != nil {
		return nil, err
	}
	return &EditFileTool{workspaceRoot: root}, nil
}

func newWorkspaceRoot(workDir string) (workspaceRoot, error) {
	abs, err := filepath.Abs(workDir)
	if err != nil {
		return workspaceRoot{}, err
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return workspaceRoot{}, err
	}
	return workspaceRoot{workDir: filepath.Clean(abs), root: root}, nil
}

func (t *WriteFileTool) Name() string { return "write_file" }

func (t *WriteFileTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name: t.Name(), Description: "Atomically create or replace a UTF-8 text file inside the workspace. Requires explicit approval.",
		InputSchema: mutableFileSchema(false), Risk: schema.RiskWrite,
	}
}

func (t *WriteFileTool) Validate(arguments json.RawMessage) error {
	var input writeFileArgs
	if err := json.Unmarshal(arguments, &input); err != nil {
		return err
	}
	if _, err := confinedRelative(input.Path); err != nil {
		return err
	}
	if len(input.Content) > maximumMutableFileBytes {
		return fmt.Errorf("content exceeds %d bytes", maximumMutableFileBytes)
	}
	return nil
}

func (t *WriteFileTool) Execute(ctx context.Context, arguments json.RawMessage) (string, error) {
	var input writeFileArgs
	if err := json.Unmarshal(arguments, &input); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	path, err := confinedRelative(input.Path)
	if err != nil {
		return "", &Error{Code: "path_outside_workspace", Message: err.Error(), Fatal: true}
	}
	unlock := lockMutablePath(t.workDir, path)
	defer unlock()
	if err := t.root.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", &Error{Code: "create_parent", Message: err.Error(), Retryable: true}
	}
	if err := atomicWrite(t.root, path, []byte(input.Content), 0o600); err != nil {
		return "", &Error{Code: "write_file", Message: err.Error(), Retryable: true}
	}
	return fmt.Sprintf(`{"path":%q,"bytes":%d}`, path, len(input.Content)), nil
}

func (t *EditFileTool) Name() string { return "edit_file" }

func (t *EditFileTool) Definition() schema.ToolDefinition {
	schemaDef := mutableFileSchema(true)
	return schema.ToolDefinition{
		Name: t.Name(), Description: "Atomically replace one uniquely identified text fragment in a workspace file. Matching degrades from exact to newline, outer-blank-line, and indentation-insensitive modes. Supply expected_sha256 from a prior read when concurrent changes are possible. Requires explicit approval.",
		InputSchema: schemaDef, Risk: schema.RiskWrite,
	}
}

func (t *EditFileTool) Validate(arguments json.RawMessage) error {
	var input editFileArgs
	if err := json.Unmarshal(arguments, &input); err != nil {
		return err
	}
	if _, err := confinedRelative(input.Path); err != nil {
		return err
	}
	if input.OldText == "" {
		return fmt.Errorf("old_text must not be empty")
	}
	if len(input.NewText) > maximumMutableFileBytes {
		return fmt.Errorf("new_text exceeds %d bytes", maximumMutableFileBytes)
	}
	if input.ExpectedSHA256 != "" {
		if len(input.ExpectedSHA256) != sha256.Size*2 {
			return fmt.Errorf("expected_sha256 must be a 64-character hexadecimal digest")
		}
		if _, err := hex.DecodeString(input.ExpectedSHA256); err != nil {
			return fmt.Errorf("expected_sha256 must be hexadecimal")
		}
	}
	return nil
}

func (t *EditFileTool) Execute(ctx context.Context, arguments json.RawMessage) (string, error) {
	var input editFileArgs
	if err := json.Unmarshal(arguments, &input); err != nil {
		return "", err
	}
	path, err := confinedRelative(input.Path)
	if err != nil {
		return "", &Error{Code: "path_outside_workspace", Message: err.Error(), Fatal: true}
	}
	unlock := lockMutablePath(t.workDir, path)
	defer unlock()
	file, err := t.root.OpenFile(path, os.O_RDONLY|nonblockFlag, 0)
	if err != nil {
		return "", &Error{Code: "open_file", Message: err.Error(), Retryable: true}
	}
	info, statErr := file.Stat()
	if statErr != nil || !info.Mode().IsRegular() || info.Size() > maximumMutableFileBytes {
		file.Close()
		return "", &Error{Code: "invalid_file", Message: "target must be a regular file no larger than 1 MiB", Fatal: true}
	}
	content, readErr := io.ReadAll(io.LimitReader(file, maximumMutableFileBytes+1))
	closeErr := file.Close()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if readErr != nil || closeErr != nil {
		return "", &Error{Code: "read_file", Message: errorsJoin(readErr, closeErr), Retryable: true}
	}
	if !utf8.Valid(content) {
		return "", &Error{Code: "invalid_file", Message: "target must contain valid UTF-8 text", Fatal: true}
	}
	originalSHA := sha256.Sum256(content)
	originalDigest := hex.EncodeToString(originalSHA[:])
	if input.ExpectedSHA256 != "" && !strings.EqualFold(input.ExpectedSHA256, originalDigest) {
		return "", &Error{Code: "edit_conflict", Message: fmt.Sprintf("target changed: expected sha256 %s, found %s; read the file again", strings.ToLower(input.ExpectedSHA256), originalDigest), Retryable: true}
	}

	original := string(content)
	updated := ""
	matchLevel := "exact"
	replacements := 1
	if input.ReplaceAll {
		count := strings.Count(original, input.OldText)
		if count == 0 {
			return "", &Error{Code: "no_match", Message: "old_text was not found; replace_all only supports exact matching", Retryable: true}
		}
		updated = strings.ReplaceAll(original, input.OldText, input.NewText)
		replacements = count
	} else {
		match, matchErr := findEditMatch(original, input.OldText)
		if matchErr != nil {
			return "", matchErr
		}
		newText := input.NewText
		if match.level == "indentation" {
			newText = reindentReplacement(newText, baseIndent(original[match.start:match.end]))
		}
		newText = convertNewlines(newText, preferredNewline(original[match.start:match.end], original))
		updated = original[:match.start] + newText + original[match.end:]
		matchLevel = match.level
	}
	if len(updated) > maximumMutableFileBytes {
		return "", &Error{Code: "file_too_large", Message: "edited file exceeds 1 MiB", Fatal: true}
	}
	if err := atomicWriteChecked(t.root, path, []byte(updated), info.Mode().Perm(), originalSHA); err != nil {
		if errorsIsEditConflict(err) {
			return "", &Error{Code: "edit_conflict", Message: "target changed while the edit was being prepared; read the file again", Retryable: true}
		}
		return "", &Error{Code: "write_file", Message: err.Error(), Retryable: true}
	}
	newSHA := sha256.Sum256([]byte(updated))
	verified, err := readRegularFile(t.root, path)
	if err != nil {
		return "", &Error{Code: "readback_failed", Message: err.Error(), Retryable: true}
	}
	verifiedSHA := sha256.Sum256(verified)
	if verifiedSHA != newSHA {
		return "", &Error{Code: "readback_mismatch", Message: "written content did not survive readback verification", Fatal: true}
	}
	result, _ := json.Marshal(map[string]any{
		"path": path, "replacements": replacements, "bytes": len(updated), "match_level": matchLevel,
		"previous_sha256": originalDigest, "sha256": hex.EncodeToString(newSHA[:]),
	})
	return string(result), nil
}

func mutableFileSchema(edit bool) map[string]any {
	properties := map[string]any{
		"path": map[string]any{"type": "string", "minLength": 1},
	}
	required := []string{"path"}
	if edit {
		properties["old_text"] = map[string]any{"type": "string", "minLength": 1}
		properties["new_text"] = map[string]any{"type": "string"}
		properties["replace_all"] = map[string]any{"type": "boolean"}
		properties["expected_sha256"] = map[string]any{
			"type": "string", "pattern": "^[0-9a-fA-F]{64}$",
			"description": "Optional SHA-256 of the file content previously read; rejects stale edits.",
		}
		required = append(required, "old_text", "new_text")
	} else {
		properties["content"] = map[string]any{"type": "string"}
		required = append(required, "content")
	}
	return map[string]any{"type": "object", "additionalProperties": false, "properties": properties, "required": required}
}

func lockMutablePath(workDir, path string) func() {
	key := filepath.Join(workDir, filepath.Clean(path))
	value, _ := mutablePathLocks.LoadOrStore(key, &sync.Mutex{})
	mutex := value.(*sync.Mutex)
	mutex.Lock()
	return mutex.Unlock
}

func findEditMatch(original, oldText string) (editMatch, error) {
	if match, count := uniqueSubstring(original, oldText); count == 1 {
		match.level = "exact"
		return match, nil
	} else if count > 1 {
		return editMatch{}, ambiguousEditError(count, "exact")
	}

	normalized, offsets := normalizeLFWithOffsets(original)
	normalizedOld := normalizeLF(oldText)
	if match, count := uniqueSubstring(normalized, normalizedOld); count == 1 {
		return editMatch{start: offsets[match.start], end: offsets[match.end], level: "newline"}, nil
	} else if count > 1 {
		return editMatch{}, ambiguousEditError(count, "newline")
	}

	trimmedOld := trimOuterBlankLines(normalizedOld)
	if trimmedOld != "" && trimmedOld != normalizedOld {
		if match, count := uniqueSubstring(normalized, trimmedOld); count == 1 {
			return editMatch{start: offsets[match.start], end: offsets[match.end], level: "outer_blank_lines"}, nil
		} else if count > 1 {
			return editMatch{}, ambiguousEditError(count, "outer_blank_lines")
		}
	}

	match, count := indentationMatch(normalized, trimmedOld)
	if count == 0 {
		return editMatch{}, &Error{Code: "no_match", Message: "old_text was not found; read the file again and provide more surrounding context", Retryable: true}
	}
	if count > 1 {
		return editMatch{}, ambiguousEditError(count, "indentation")
	}
	return editMatch{start: offsets[match.start], end: offsets[match.end], level: "indentation"}, nil
}

func uniqueSubstring(content, fragment string) (editMatch, int) {
	if fragment == "" {
		return editMatch{}, 0
	}
	first := strings.Index(content, fragment)
	if first < 0 {
		return editMatch{}, 0
	}
	count := 1
	for offset := first + len(fragment); offset <= len(content); {
		next := strings.Index(content[offset:], fragment)
		if next < 0 {
			break
		}
		count++
		offset += next + len(fragment)
	}
	return editMatch{start: first, end: first + len(fragment)}, count
}

func ambiguousEditError(count int, level string) error {
	return &Error{Code: "ambiguous_match", Message: fmt.Sprintf("old_text matched %d times at %s level; provide more surrounding context", count, level), Retryable: true}
}

func normalizeLF(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "\r\n", "\n"), "\r", "\n")
}

func normalizeLFWithOffsets(value string) (string, []int) {
	var builder strings.Builder
	builder.Grow(len(value))
	offsets := make([]int, 1, len(value)+1)
	for index := 0; index < len(value); {
		if value[index] == '\r' {
			builder.WriteByte('\n')
			if index+1 < len(value) && value[index+1] == '\n' {
				index += 2
			} else {
				index++
			}
			offsets = append(offsets, index)
			continue
		}
		builder.WriteByte(value[index])
		index++
		offsets = append(offsets, index)
	}
	return builder.String(), offsets
}

func trimOuterBlankLines(value string) string {
	lines := strings.Split(value, "\n")
	start, end := 0, len(lines)
	for start < end && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	for end > start && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	return strings.Join(lines[start:end], "\n")
}

func indentationMatch(content, oldText string) (editMatch, int) {
	oldText = trimOuterBlankLines(oldText)
	if oldText == "" {
		return editMatch{}, 0
	}
	contentLines := strings.Split(content, "\n")
	oldLines := strings.Split(oldText, "\n")
	if len(oldLines) > len(contentLines) {
		return editMatch{}, 0
	}
	lineOffsets := make([]int, len(contentLines)+1)
	for index, line := range contentLines {
		lineOffsets[index+1] = lineOffsets[index] + len(line)
		if index < len(contentLines)-1 {
			lineOffsets[index+1]++
		}
	}
	count := 0
	match := editMatch{}
	for start := 0; start <= len(contentLines)-len(oldLines); start++ {
		matched := true
		for offset := range oldLines {
			if strings.TrimSpace(contentLines[start+offset]) != strings.TrimSpace(oldLines[offset]) {
				matched = false
				break
			}
		}
		if matched {
			count++
			endLine := start + len(oldLines)
			end := lineOffsets[endLine]
			if endLine < len(contentLines) {
				end--
			}
			match = editMatch{start: lineOffsets[start], end: end}
		}
	}
	return match, count
}

func baseIndent(value string) string {
	for _, line := range strings.Split(normalizeLF(value), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		return line[:len(line)-len(strings.TrimLeft(line, " \t"))]
	}
	return ""
}

func reindentReplacement(value, targetIndent string) string {
	normalized := normalizeLF(value)
	lines := strings.Split(normalized, "\n")
	common := -1
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		width := len(line) - len(strings.TrimLeft(line, " \t"))
		if common < 0 || width < common {
			common = width
		}
	}
	if common < 0 {
		return value
	}
	for index, line := range lines {
		if strings.TrimSpace(line) == "" {
			lines[index] = ""
			continue
		}
		strip := common
		if strip > len(line) {
			strip = len(line)
		}
		lines[index] = targetIndent + line[strip:]
	}
	return strings.Join(lines, "\n")
}

func preferredNewline(fragment, whole string) string {
	if strings.Contains(fragment, "\r\n") || (!strings.Contains(fragment, "\n") && strings.Contains(whole, "\r\n")) {
		return "\r\n"
	}
	return "\n"
}

func convertNewlines(value, newline string) string {
	normalized := normalizeLF(value)
	if newline == "\n" {
		return normalized
	}
	return strings.ReplaceAll(normalized, "\n", newline)
}

func confinedRelative(path string) (string, error) {
	if strings.TrimSpace(path) == "" || filepath.IsAbs(path) {
		return "", fmt.Errorf("path must be non-empty and relative")
	}
	clean := filepath.Clean(path)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes workspace", path)
	}
	return clean, nil
}

func atomicWrite(root *os.Root, path string, content []byte, mode os.FileMode) error {
	return atomicWriteChecked(root, path, content, mode, [sha256.Size]byte{})
}

func atomicWriteChecked(root *os.Root, path string, content []byte, mode os.FileMode, expected [sha256.Size]byte) error {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return err
	}
	temporary := filepath.Join(filepath.Dir(path), ".01agent-"+hex.EncodeToString(suffix[:])+".tmp")
	file, err := root.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(content)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errorsJoinError(writeErr, syncErr, closeErr); err != nil {
		_ = root.Remove(temporary)
		return err
	}
	if expected != ([sha256.Size]byte{}) {
		current, err := readRegularFile(root, path)
		if err != nil {
			_ = root.Remove(temporary)
			return err
		}
		if sha256.Sum256(current) != expected {
			_ = root.Remove(temporary)
			return errEditConflict
		}
	}
	if err := root.Rename(temporary, path); err != nil {
		_ = root.Remove(temporary)
		return err
	}
	return nil
}

func readRegularFile(root *os.Root, path string) ([]byte, error) {
	file, err := root.OpenFile(path, os.O_RDONLY|nonblockFlag, 0)
	if err != nil {
		return nil, err
	}
	info, statErr := file.Stat()
	if statErr != nil || !info.Mode().IsRegular() || info.Size() > maximumMutableFileBytes {
		_ = file.Close()
		if statErr != nil {
			return nil, statErr
		}
		return nil, errors.New("target must be a regular file no larger than 1 MiB")
	}
	content, readErr := io.ReadAll(io.LimitReader(file, maximumMutableFileBytes+1))
	closeErr := file.Close()
	return content, errorsJoinError(readErr, closeErr)
}

func errorsIsEditConflict(err error) bool { return errors.Is(err, errEditConflict) }

func errorsJoin(items ...error) string {
	if err := errorsJoinError(items...); err != nil {
		return err.Error()
	}
	return ""
}

func errorsJoinError(items ...error) error {
	var messages []string
	for _, item := range items {
		if item != nil {
			messages = append(messages, item.Error())
		}
	}
	if len(messages) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(messages, "; "))
}

package github

import (
	"fmt"
	"strconv"
	"strings"
)

type filePatchInput struct {
	Operation           string
	Content             string
	Search              string
	Replace             string
	ExpectedOccurrences int
	StartLine           int
	EndLine             int
	Replacement         string
	Patch               string
}

type filePatchPreview struct {
	Operation          string   `json:"operation"`
	MatchedOccurrences int      `json:"matched_occurrences"`
	ChangedLineCount   int      `json:"changed_line_count"`
	Hunks              []string `json:"hunks"`
}

func applyFilePatch(before string, input filePatchInput) (string, filePatchPreview, error) {
	operation := input.Operation
	if operation == "" {
		operation = "replace"
	}
	preview := filePatchPreview{Operation: operation}

	var after string
	switch operation {
	case "replace":
		after = input.Content
		preview.MatchedOccurrences = 1
	case "patch_text":
		if input.Search == "" {
			return "", preview, fmt.Errorf("search is required for patch_text")
		}
		matches := strings.Count(before, input.Search)
		preview.MatchedOccurrences = matches
		expected := input.ExpectedOccurrences
		if expected == 0 {
			expected = 1
		}
		if matches != expected {
			return "", preview, fmt.Errorf("search/replace expected %d occurrence(s), found %d", expected, matches)
		}
		after = strings.ReplaceAll(before, input.Search, input.Replace)
	case "patch_range":
		lines := splitFileLines(before)
		if input.StartLine < 1 || input.EndLine < input.StartLine || input.EndLine > len(lines) {
			return "", preview, fmt.Errorf("start_line and end_line must identify an existing non-empty range")
		}
		replacement := splitFileLines(input.Replacement)
		afterLines := append([]string{}, lines[:input.StartLine-1]...)
		afterLines = append(afterLines, replacement...)
		afterLines = append(afterLines, lines[input.EndLine:]...)
		after = strings.Join(afterLines, "\n")
		preview.MatchedOccurrences = 1
		preview.Hunks = []string{fmt.Sprintf("lines %d-%d", input.StartLine, input.EndLine)}
	case "unified_diff":
		var err error
		after, preview, err = applyUnifiedDiff(before, input.Patch)
		if err != nil {
			return "", preview, err
		}
	default:
		return "", preview, fmt.Errorf("operation must be one of replace, patch_text, patch_range, unified_diff")
	}

	if operation != "replace" {
		preview.ChangedLineCount = changedLineCount(before, after)
		if strings.TrimSpace(after) == "" {
			return "", preview, fmt.Errorf("resulting file is empty")
		}
		if len(before) >= 512 && len(after) < len(before)/8 {
			return "", preview, fmt.Errorf("resulting file looks accidentally truncated")
		}
	}
	return after, preview, nil
}

func unrelatedFileContentPreserved(before, after string, input filePatchInput) bool {
	if input.Operation == "replace" || before == after {
		return true
	}
	switch input.Operation {
	case "patch_text":
		at := strings.Index(before, input.Search)
		if at < 0 {
			return false
		}
		return strings.HasPrefix(after, before[:at]) && strings.HasSuffix(after, before[at+len(input.Search):])
	case "patch_range":
		beforeLines, afterLines := splitFileLines(before), splitFileLines(after)
		prefix := beforeLines[:input.StartLine-1]
		suffix := beforeLines[input.EndLine:]
		if len(afterLines) < len(prefix)+len(suffix) {
			return false
		}
		return strings.Join(afterLines[:len(prefix)], "\n") == strings.Join(prefix, "\n") && strings.Join(afterLines[len(afterLines)-len(suffix):], "\n") == strings.Join(suffix, "\n")
	default:
		beforeLines, afterLines := splitFileLines(before), splitFileLines(after)
		if len(beforeLines) == 0 || len(afterLines) == 0 {
			return false
		}
		return strings.Contains(after, beforeLines[0]) && strings.Contains(after, beforeLines[len(beforeLines)-1])
	}
}

func splitFileLines(value string) []string {
	return strings.Split(value, "\n")
}

func changedLineCount(before, after string) int {
	a := splitFileLines(before)
	b := splitFileLines(after)
	count := 0
	for i := 0; i < len(a) || i < len(b); i++ {
		var left, right string
		if i < len(a) {
			left = a[i]
		}
		if i < len(b) {
			right = b[i]
		}
		if left != right {
			count++
		}
	}
	return count
}

type unifiedHunk struct {
	oldStart int
	oldCount int
	newStart int
	newCount int
	lines    []string
}

func applyUnifiedDiff(before, patch string) (string, filePatchPreview, error) {
	preview := filePatchPreview{Operation: "unified_diff"}
	if strings.TrimSpace(patch) == "" {
		return "", preview, fmt.Errorf("patch is required")
	}
	lines := strings.Split(strings.ReplaceAll(patch, "\r\n", "\n"), "\n")
	var hunks []unifiedHunk
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ ") || strings.HasPrefix(line, "diff ") || strings.HasPrefix(line, "index ") {
			continue
		}
		if !strings.HasPrefix(line, "@@ ") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			return "", preview, fmt.Errorf("malformed unified diff line %q", line)
		}
		hunk, err := parseUnifiedHunkHeader(line)
		if err != nil {
			return "", preview, err
		}
		i++
		for oldSeen, newSeen := 0, 0; i < len(lines); i++ {
			entry := lines[i]
			if strings.HasPrefix(entry, "@@ ") {
				i--
				break
			}
			if entry == "" && i == len(lines)-1 {
				break
			}
			if entry == "\\ No newline at end of file" {
				continue
			}
			if entry == "" || (entry[0] != ' ' && entry[0] != '-' && entry[0] != '+') {
				return "", preview, fmt.Errorf("malformed unified diff hunk line %q", entry)
			}
			hunk.lines = append(hunk.lines, entry)
			switch entry[0] {
			case ' ':
				oldSeen++
				newSeen++
			case '-':
				oldSeen++
			case '+':
				newSeen++
			}
			if oldSeen > hunk.oldCount || newSeen > hunk.newCount {
				return "", preview, fmt.Errorf("unified diff hunk counts exceeded")
			}
		}
		if countOldNew(hunk.lines) != [2]int{hunk.oldCount, hunk.newCount} {
			return "", preview, fmt.Errorf("unified diff hunk counts do not match")
		}
		hunks = append(hunks, hunk)
	}
	if len(hunks) == 0 {
		return "", preview, fmt.Errorf("unified diff contains no hunks")
	}

	beforeLines := splitFileLines(before)
	var out []string
	oldCursor := 1
	lastEnd := 0
	for _, hunk := range hunks {
		if hunk.oldStart < oldCursor || hunk.oldStart <= lastEnd {
			return "", preview, fmt.Errorf("unified diff hunks overlap or are out of order")
		}
		if hunk.oldStart < 1 || hunk.oldStart+hunk.oldCount-1 > len(beforeLines) {
			return "", preview, fmt.Errorf("unified diff hunk is outside the file")
		}
		out = append(out, beforeLines[oldCursor-1:hunk.oldStart-1]...)
		pos := hunk.oldStart - 1
		for _, entry := range hunk.lines {
			text := entry[1:]
			switch entry[0] {
			case ' ':
				if beforeLines[pos] != text {
					return "", preview, fmt.Errorf("unified diff context does not match at line %d", pos+1)
				}
				out = append(out, text)
				pos++
			case '-':
				if beforeLines[pos] != text {
					return "", preview, fmt.Errorf("unified diff removal does not match at line %d", pos+1)
				}
				pos++
			case '+':
				out = append(out, text)
			}
		}
		oldCursor = pos + 1
		lastEnd = pos
		preview.MatchedOccurrences++
		preview.Hunks = append(preview.Hunks, fmt.Sprintf("-%d,%d +%d,%d", hunk.oldStart, hunk.oldCount, hunk.newStart, hunk.newCount))
	}
	out = append(out, beforeLines[oldCursor-1:]...)
	return strings.Join(out, "\n"), preview, nil
}

func countOldNew(lines []string) [2]int {
	var result [2]int
	for _, line := range lines {
		if line[0] != '+' {
			result[0]++
		}
		if line[0] != '-' {
			result[1]++
		}
	}
	return result
}

func parseUnifiedHunkHeader(line string) (unifiedHunk, error) {
	var h unifiedHunk
	parts := strings.Fields(strings.TrimSuffix(strings.TrimPrefix(line, "@@ "), " @@"))
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "-") || !strings.HasPrefix(parts[1], "+") {
		return h, fmt.Errorf("malformed unified diff hunk header %q", line)
	}
	parse := func(value string) (int, int, error) {
		value = value[1:]
		pieces := strings.SplitN(value, ",", 2)
		start, err := strconv.Atoi(pieces[0])
		if err != nil || start < 1 {
			return 0, 0, fmt.Errorf("malformed unified diff line range %q", value)
		}
		count := 1
		if len(pieces) == 2 {
			count, err = strconv.Atoi(pieces[1])
			if err != nil || count < 0 {
				return 0, 0, fmt.Errorf("malformed unified diff line count %q", value)
			}
		}
		return start, count, nil
	}
	var err error
	if h.oldStart, h.oldCount, err = parse(parts[0]); err != nil {
		return h, err
	}
	if h.newStart, h.newCount, err = parse(parts[1]); err != nil {
		return h, err
	}
	return h, nil
}

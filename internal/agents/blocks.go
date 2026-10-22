package agents

import (
	"fmt"
	"os"
	"strings"
)

func containsLine(s, line string) bool {
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimRight(l, "\r") == line {
			return true
		}
	}
	return false
}

// findBlock locates the managed block: start is the beginning of the begin
// marker line, end is just past the end marker line (including its newline).
func findBlock(content string) (start, end int, found bool, err error) {
	start = -1
	for off := 0; off <= len(content); {
		i := strings.Index(content[off:], blockBegin)
		if i < 0 {
			break
		}
		i += off
		if i == 0 || content[i-1] == '\n' {
			start = i
			break
		}
		off = i + len(blockBegin)
	}
	if start < 0 {
		return 0, 0, false, nil
	}
	j := strings.Index(content[start:], blockEnd)
	if j < 0 {
		return 0, 0, false, fmt.Errorf("found %q without %q; fix or remove the marker by hand, then rerun", blockBegin, blockEnd)
	}
	end = start + j + len(blockEnd)
	if end < len(content) && content[end] == '\r' {
		end++
	}
	if end < len(content) && content[end] == '\n' {
		end++
	}
	return start, end, true, nil
}

func installBlock(path string) (string, string, error) {
	cur, exists, err := readIfExists(path)
	if err != nil {
		return "", "", err
	}
	content := string(cur)
	start, end, found, err := findBlock(content)
	if err != nil {
		return "", "", err
	}
	want := blockText()
	var next string
	detail := ""
	if found {
		if strings.TrimRight(content[start:end], "\r\n") == want {
			return StateUnchanged, "", nil
		}
		next = content[:start] + want + "\n" + content[end:]
		detail = "updated"
	} else {
		next = content
		if next != "" {
			if !strings.HasSuffix(next, "\n") {
				next += "\n"
			}
			next += "\n"
		}
		next += want + "\n"
	}
	_ = exists
	if err := writeKeepMode(path, []byte(next), 0o644, 0o700); err != nil {
		return "", "", err
	}
	return StateInstalled, detail, nil
}

func statusBlock(path string) (string, string, error) {
	cur, exists, err := readIfExists(path)
	if err != nil {
		return "", "", err
	}
	if !exists {
		return StateMissing, "", nil
	}
	start, end, found, err := findBlock(string(cur))
	if err != nil {
		return "", "", err
	}
	if !found {
		return StateMissing, "", nil
	}
	if strings.TrimRight(string(cur)[start:end], "\r\n") != blockText() {
		return StateOutdated, "run `kran agents install` to update it", nil
	}
	return StateInstalled, "", nil
}

func removeBlock(path string) (string, string, error) {
	cur, exists, err := readIfExists(path)
	if err != nil {
		return "", "", err
	}
	if !exists {
		return StateUnchanged, "not installed", nil
	}
	content := string(cur)
	start, end, found, err := findBlock(content)
	if err != nil {
		return "", "", err
	}
	if !found {
		return StateUnchanged, "not installed", nil
	}
	before, after := content[:start], content[end:]
	// Install put one blank line between existing text and the block.
	if strings.HasSuffix(before, "\n\n") {
		before = before[:len(before)-1]
	}
	next := before + after
	if strings.TrimSpace(next) == "" {
		if err := os.Remove(resolve(path)); err != nil {
			return "", "", err
		}
		return StateRemoved, "file was only kran's block; deleted", nil
	}
	if err := writeKeepMode(path, []byte(next), 0o644, 0o700); err != nil {
		return "", "", err
	}
	return StateRemoved, "", nil
}

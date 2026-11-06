// Package config loads optional local configuration at startup.
//
// A .env file is UTF-8 text with one KEY=value assignment per line. Keys are
// shell-style ASCII identifiers: [A-Za-z_][A-Za-z0-9_]*. Blank lines and lines
// whose first non-whitespace character is # are ignored. Whitespace around
// keys and values is trimmed. An unquoted value is literal, including # and
// quotes not at the start; there are no inline comments or variable expansions.
// A value starting with a quote must be entirely enclosed in matching quotes:
// single-quoted contents are literal, while double-quoted contents use Go
// strconv.Unquote escapes. Literal newlines cannot occur within a value, but
// double-quoted escapes such as \n can represent them. Prefer single quotes
// around Windows paths and special passwords to avoid interpreting backslashes.
// Single-quoted values cannot contain another single quote. A UTF-8 BOM at
// the start and CRLF line endings are supported. The file is limited to 1 MiB;
// the scanner limit is 64 KiB per line, including its line ending. A missing file
// is ignored; unreadable or invalid files prevent startup. Duplicate keys are
// errors (case-insensitive on Windows), as are invalid assignments, even when
// the corresponding OS variable is already set. Existing OS variables,
// including empty ones, take precedence.
package config

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	maxFileSize = 1 << 20  // 1 MiB
	maxLineSize = 64 << 10 // 64 KiB, including the line ending
)

type assignment struct {
	key   string
	value string
	line  int
}

// LoadEnv loads .env from the current working directory if it exists.
// The whole file is validated before any environment variable is changed.
func LoadEnv() error {
	return loadFile(".env")
}

func loadFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// A dangling symlink is an existing but unreadable configuration.
			if _, statErr := os.Lstat(path); errors.Is(statErr, os.ErrNotExist) {
				return nil
			}
		}
		return fmt.Errorf("cannot open .env: %w", err)
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxFileSize+1))
	if err != nil {
		return fmt.Errorf("cannot read .env: %w", err)
	}
	if len(data) > maxFileSize {
		return fmt.Errorf(".env exceeds the %d-byte file limit", maxFileSize)
	}

	assignments, err := parse(data)
	if err != nil {
		return err
	}

	var added []string
	for _, a := range assignments {
		if _, exists := os.LookupEnv(a.key); exists {
			continue
		}
		if err := os.Setenv(a.key, a.value); err != nil {
			for _, key := range added {
				_ = os.Unsetenv(key)
			}
			return fmt.Errorf(".env line %d: cannot set environment variable: %w", a.line, err)
		}
		added = append(added, a.key)
	}
	return nil
}

func parse(data []byte) ([]assignment, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), maxLineSize)
	var assignments []assignment
	seen := make(map[string]int)
	line := 0
	for scanner.Scan() {
		line++
		text := scanner.Text()
		if !utf8.ValidString(text) {
			return nil, lineError(line, "invalid UTF-8")
		}
		text = strings.TrimSpace(text)
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		key, value, ok := strings.Cut(text, "=")
		if !ok {
			return nil, lineError(line, "expected KEY=value")
		}
		key = strings.TrimSpace(key)
		if !validKey(key) {
			return nil, lineError(line, "invalid key; use a shell-style identifier")
		}
		duplicateKey := key
		if runtime.GOOS == "windows" {
			duplicateKey = strings.ToUpper(key)
		}
		if first, exists := seen[duplicateKey]; exists {
			return nil, lineError(line, fmt.Sprintf("duplicate key (first defined on line %d)", first))
		}
		seen[duplicateKey] = line
		value, err := parseValue(strings.TrimSpace(value))
		if err != nil {
			return nil, lineError(line, err.Error())
		}
		assignments = append(assignments, assignment{key: key, value: value, line: line})
	}
	if err := scanner.Err(); err != nil {
		return nil, lineError(line+1, fmt.Sprintf("line exceeds the %d-byte limit", maxLineSize))
	}
	return assignments, nil
}

func validKey(key string) bool {
	for i := 0; i < len(key); i++ {
		c := key[i]
		if c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || i > 0 && c >= '0' && c <= '9' {
			continue
		}
		return false
	}
	return key != ""
}

func parseValue(value string) (string, error) {
	if strings.ContainsRune(value, '\r') {
		return "", errors.New("literal carriage returns are not allowed in values")
	}
	if strings.HasPrefix(value, "'") {
		if len(value) < 2 || value[len(value)-1] != '\'' || strings.ContainsRune(value[1:len(value)-1], '\'') {
			return "", errors.New("invalid single-quoted value")
		}
		value = value[1 : len(value)-1]
	} else if strings.HasPrefix(value, "\"") {
		var err error
		value, err = strconv.Unquote(value)
		if err != nil {
			return "", errors.New("invalid double-quoted value or escape")
		}
	}
	if strings.ContainsRune(value, 0) {
		return "", errors.New("NUL is not allowed in values")
	}
	return value, nil
}

func lineError(line int, reason string) error {
	return fmt.Errorf(".env line %d: %s", line, reason)
}

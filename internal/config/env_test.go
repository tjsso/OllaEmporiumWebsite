package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func unsetForTest(t *testing.T, keys ...string) {
	t.Helper()
	for _, key := range keys {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
}

func envFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadEnvFromWorkingDirectory(t *testing.T) {
	const key = "OLLA_TEST_WORKING_DIR_ENV_9251"
	unsetForTest(t, key)
	dir := t.TempDir()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(previous); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})

	if err := LoadEnv(); err != nil {
		t.Fatalf("missing .env: %v", err)
	}
	if _, ok := os.LookupEnv(key); ok {
		t.Fatal("missing file changed environment")
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(key+"=local\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := LoadEnv(); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv(key); got != "local" {
		t.Errorf("loaded value = %q, want local", got)
	}
}

func TestLoadPrecedenceAndFormat(t *testing.T) {
	const prefix = "OLLA_TEST_ENV_FORMAT_9251_"
	keys := []string{"EXISTING", "EMPTY", "PLAIN", "SINGLE", "DOUBLE", "BLANK", "QUOTES", "UNICODE"}
	for _, key := range keys {
		unsetForTest(t, prefix+key)
	}
	t.Setenv(prefix+"EXISTING", "from OS")
	t.Setenv(prefix+"EMPTY", "")

	contents := "\xef\xbb\xbf  # comment\r\n\t\r\n" +
		prefix + "EXISTING=file\r\n" +
		prefix + "EMPTY=file\r\n" +
		"  " + prefix + "PLAIN  =  hello # literal = $NOT_EXPANDED  \r\n" +
		prefix + `SINGLE = ' C:\Users\example\a#b $d " ' ` + "\r\n" +
		prefix + `DOUBLE = "escaped\n\t\"quote\"\\path\x21"` + "\r\n" +
		prefix + "BLANK=   \r\n" +
		prefix + `QUOTES=word'"word` + "\r\n" +
		prefix + "UNICODE=café"
	if err := loadFile(envFile(t, contents)); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"EXISTING": "from OS",
		"EMPTY":    "",
		"PLAIN":    "hello # literal = $NOT_EXPANDED",
		"SINGLE":   ` C:\Users\example\a#b $d " `,
		"DOUBLE":   "escaped\n\t\"quote\"\\path!",
		"BLANK":    "",
		"QUOTES":   `word'"word`,
		"UNICODE":  "café",
	}
	for key, expected := range want {
		got, ok := os.LookupEnv(prefix + key)
		if !ok || got != expected {
			t.Errorf("%s = %q (present %t), want %q", key, got, ok, expected)
		}
	}
}

func TestInvalidFilesDoNotPartiallyLoad(t *testing.T) {
	const key = "OLLA_TEST_ENV_PARTIAL_9251"
	const sensitive = "VALUE_THAT_MUST_NOT_BE_PRINTED_9251"
	tests := []struct {
		name   string
		bad    string
		reason string
	}{
		{"missing equals", sensitive, "expected KEY=value"},
		{"empty key", "=" + sensitive, "invalid key"},
		{"digit key", "1KEY=" + sensitive, "invalid key"},
		{"dash key", "BAD-KEY=" + sensitive, "invalid key"},
		{"space in key", "BAD KEY=" + sensitive, "invalid key"},
		{"export unsupported", "export KEY=" + sensitive, "invalid key"},
		{"unclosed single quote", "BAD='" + sensitive, "single-quoted"},
		{"multiline value", "BAD='" + sensitive + "\ncontinued'", "single-quoted"},
		{"extra single quote", "BAD='" + sensitive + "'extra", "single-quoted"},
		{"inner single quote", "BAD='" + sensitive + "'x'", "single-quoted"},
		{"single inline comment", "BAD='" + sensitive + "' # comment", "single-quoted"},
		{"unclosed double quote", "BAD=\"" + sensitive, "double-quoted"},
		{"double inline comment", "BAD=\"" + sensitive + "\" # comment", "double-quoted"},
		{"invalid escape", `BAD="` + sensitive + `\q"`, "double-quoted"},
		{"raw NUL", "BAD=" + sensitive + "\x00", "NUL"},
		{"escaped NUL", `BAD="` + sensitive + `\x00"`, "NUL"},
		{"raw carriage return", "BAD=" + sensitive + "\rX", "carriage returns"},
		{"invalid UTF-8", "BAD=" + sensitive + "\xff", "UTF-8"},
		{"duplicate", key + "=" + sensitive, "duplicate key (first defined on line 1)"},
	}
	if runtime.GOOS == "windows" {
		tests = append(tests, struct {
			name   string
			bad    string
			reason string
		}{"case-insensitive duplicate", strings.ToLower(key) + "=" + sensitive, "duplicate key"})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			unsetForTest(t, key)
			path := envFile(t, key+"=first\n# comment\n"+tt.bad+"\n")
			err := loadFile(path)
			if err == nil || !strings.Contains(err.Error(), "line 3") || !strings.Contains(err.Error(), tt.reason) {
				t.Fatalf("error = %v, want line 3 and %q", err, tt.reason)
			}
			if strings.Contains(err.Error(), sensitive) || strings.Contains(err.Error(), "first\n") {
				t.Fatalf("error reveals a value: %v", err)
			}
			if _, ok := os.LookupEnv(key); ok {
				t.Error("invalid file partially changed environment")
			}
		})
	}
}

func TestInvalidAssignmentsAreCheckedEvenWhenSet(t *testing.T) {
	const key = "OLLA_TEST_ENV_SET_INVALID_9251"
	t.Setenv(key, "existing")
	for _, contents := range []string{key + `="bad\q"`, key + "=one\n" + key + "=two"} {
		if err := loadFile(envFile(t, contents)); err == nil {
			t.Error("invalid assignment for existing variable was accepted")
		}
		if got := os.Getenv(key); got != "existing" {
			t.Errorf("existing variable changed: %q", got)
		}
	}
}

func TestIdentifierAndEmptyQuotedValues(t *testing.T) {
	assignments, err := parse([]byte("_1=''\nA2=\"\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(assignments) != 2 || assignments[0].key != "_1" || assignments[0].value != "" || assignments[1].key != "A2" || assignments[1].value != "" {
		t.Fatalf("unexpected assignments: %+v", assignments)
	}
}

func TestUnreadableFile(t *testing.T) {
	// Reading a directory fails on all supported platforms, including when tests
	// run with privileges that would bypass file permission bits.
	if err := loadFile(t.TempDir()); err == nil || !strings.Contains(err.Error(), ".env") {
		t.Fatalf("unreadable file error = %v", err)
	}
}

func TestInputLimits(t *testing.T) {
	const key = "OLLA_TEST_ENV_LIMIT_9251"
	unsetForTest(t, key)
	for _, tt := range []struct {
		name, contents, want string
	}{
		{"line", key + "=first\nLONG=" + strings.Repeat("x", maxLineSize), "line 2"},
		{"file", key + "=first\n" + strings.Repeat("# comment\n", maxFileSize/10+1), fmt.Sprintf("%d-byte file limit", maxFileSize)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := loadFile(envFile(t, tt.contents))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
			if _, ok := os.LookupEnv(key); ok {
				t.Error("oversized file partially changed environment")
			}
		})
	}
}

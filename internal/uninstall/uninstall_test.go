package uninstall

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStripPathLinesExactMatch(t *testing.T) {
	dir := t.TempDir()
	profile := filepath.Join(dir, ".profile")
	executableDir := "/opt/lofi/bin"
	input := strings.Join([]string{
		`export PATH="$PATH:/opt/lofi/bin"`,
		`export PATH="$PATH:/usr/local/bin"`,
	}, "\n")
	if err := os.WriteFile(profile, []byte(input), 0o644); err != nil {
		t.Fatalf("write profile: %v", err)
	}

	changed, err := stripPathLines(profile, executableDir)
	if err != nil {
		t.Fatalf("stripPathLines returned error: %v", err)
	}
	if !changed {
		t.Fatalf("expected changed=true")
	}

	out, err := os.ReadFile(profile)
	if err != nil {
		t.Fatalf("read profile: %v", err)
	}
	text := string(out)
	if strings.Contains(strings.ToLower(text), strings.ToLower(executableDir)) {
		t.Fatalf("expected executable path to be removed, got %q", text)
	}
	if !strings.Contains(text, `/usr/local/bin`) {
		t.Fatalf("expected unrelated PATH line to remain, got %q", text)
	}
}

func TestStripPathLinesCommentOnlyMention(t *testing.T) {
	dir := t.TempDir()
	profile := filepath.Join(dir, ".profile")
	executableDir := "/opt/lofi/bin"
	input := strings.Join([]string{
		`# path hint: /opt/lofi/bin`,
		`export FOO="bar"`,
	}, "\n")
	if err := os.WriteFile(profile, []byte(input), 0o644); err != nil {
		t.Fatalf("write profile: %v", err)
	}

	changed, err := stripPathLines(profile, executableDir)
	if err != nil {
		t.Fatalf("stripPathLines returned error: %v", err)
	}
	if changed {
		t.Fatalf("expected changed=false for comment-only mention")
	}
}

func TestStripPathLinesMissingProfile(t *testing.T) {
	dir := t.TempDir()
	profile := filepath.Join(dir, ".missing-profile")

	changed, err := stripPathLines(profile, "/opt/lofi/bin")
	if err != nil {
		t.Fatalf("expected nil error for missing profile, got %v", err)
	}
	if changed {
		t.Fatalf("expected changed=false for missing profile")
	}
}

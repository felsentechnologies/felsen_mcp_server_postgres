package version

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestDefaultVersionMatchesRepository(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	want := strings.TrimSpace(string(data))
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(want) {
		t.Fatalf("VERSION is not stable SemVer: %q", want)
	}
	if Version != want {
		t.Fatalf("default Go version %q does not match VERSION file %q", Version, want)
	}
}

func TestBuildStringIncludesBuildMetadata(t *testing.T) {
	originalCommit, originalDate := Commit, Date
	t.Cleanup(func() {
		Commit, Date = originalCommit, originalDate
	})
	Commit = "abc123"
	Date = "2026-08-08T00:00:00Z"
	if got, want := BuildString(), Version+" (abc123, 2026-08-08T00:00:00Z)"; got != want {
		t.Fatalf("BuildString() = %q, want %q", got, want)
	}
}

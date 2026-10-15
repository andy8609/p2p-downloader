package version

import (
	"strings"
	"testing"
)

func TestFullContainsDefaults(t *testing.T) {
	got := Full()
	for _, want := range []string{Version, Commit, BuildTime, "go", "windows"} {
		if !strings.Contains(got, want) {
			t.Errorf("Full() = %q, 期望包含 %q", got, want)
		}
	}
}

func TestPlatformHasSlash(t *testing.T) {
	if !strings.Contains(Platform(), "/") {
		t.Fatalf("Platform() = %q, 期望 os/arch 形式", Platform())
	}
}

func TestBuildInfoIsOverridable(t *testing.T) {
	origV, origC := Version, Commit
	defer func() { Version, Commit = origV, origC }()

	Version, Commit = "1.2.3", "abc1234"
	got := Full()
	if !strings.HasPrefix(got, "1.2.3 (commit abc1234") {
		t.Fatalf("Full() = %q, 未反映被注入的版本与提交", got)
	}
}

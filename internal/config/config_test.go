package config

import (
	"path/filepath"
	"testing"

	"github.com/luigi23997/azemu/internal/host"
)

func TestAbs(t *testing.T) {
	c := &Config{Root: filepath.FromSlash("/proj")}
	if got, want := c.Abs("apps/x"), filepath.Join(c.Root, "apps", "x"); got != want {
		t.Errorf("relative: %q, want %q", got, want)
	}
	if got := c.Abs(filepath.FromSlash("/abs/x")); got != filepath.FromSlash("/abs/x") {
		t.Errorf("absolute: %q", got)
	}
	if host.GOOS() == "windows" {
		for _, p := range []string{`C:\data\x`, "c:/data/x"} {
			if got := c.Abs(p); got != p {
				t.Errorf("drive path %q was joined to the root: %q", p, got)
			}
		}
	}
}

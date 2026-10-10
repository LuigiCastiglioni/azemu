package tfstatic

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// moduleSources maps remote (registry/git) module calls to directories using the modules.json
// that `terraform init` writes into the azemu-owned data dir. Local sources never come here.
type moduleSources struct {
	ctx     context.Context
	root    string // terraform root dir (init runs here)
	dataDir string
	inited  bool
}

type modulesJSON struct {
	Modules []struct{ Key, Source, Dir string }
}

func (s *moduleSources) lookup(key, source string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(s.dataDir, "modules", "modules.json"))
	if err != nil {
		return "", false
	}
	var mj modulesJSON
	if json.Unmarshal(b, &mj) != nil {
		return "", false
	}
	norm := func(x string) string { return strings.TrimPrefix(x, "registry.terraform.io/") }
	for _, m := range mj.Modules {
		// a recorded source that differs from the config means the manifest is stale
		if m.Key == key && (norm(m.Source) == norm(source) || s.inited) {
			if filepath.IsAbs(m.Dir) {
				return m.Dir, true
			}
			return filepath.Join(s.root, m.Dir), true
		}
	}
	return "", false
}

// dir returns the directory of the remote module at key, running `terraform init` (once) if the
// manifest is missing or does not match.
func (s *moduleSources) dir(key, source string) (string, error) {
	if d, ok := s.lookup(key, source); ok {
		return d, nil
	}
	if !s.inited {
		s.inited = true
		if s.dataDir == "" {
			return "", fmt.Errorf("remote module source %q needs Options.DataDir", source)
		}
		cmd := exec.CommandContext(s.ctx, "terraform", "init", "-backend=false", "-input=false", "-no-color")
		cmd.Dir = s.root
		cmd.Env = append(os.Environ(), "TF_DATA_DIR="+s.dataDir, "TF_IN_AUTOMATION=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			tail := strings.TrimSpace(string(out))
			if len(tail) > 400 {
				tail = tail[len(tail)-400:]
			}
			return "", fmt.Errorf("terraform init for remote module %q: %w: %s", source, err, tail)
		}
		if d, ok := s.lookup(key, source); ok {
			return d, nil
		}
	}
	return "", fmt.Errorf("remote module %q (%s) not found in modules.json", key, source)
}

package stack

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// WriteEnvFile writes env as sorted KEY=VALUE lines (mode 0600, dirs 0700).
// Values are raw: compose reads the file with `format: raw`, so no quoting or interpolation applies.
func WriteEnvFile(path string, env map[string]string) error {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		if strings.ContainsAny(env[k], "\r\n") {
			return fmt.Errorf("env var %s: value contains a newline, which an env file cannot carry", k)
		}
		if k == "" || strings.ContainsAny(k, "=\r\n") {
			return fmt.Errorf("invalid env var name %q", k)
		}
		b.WriteString(k + "=" + env[k] + "\n")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600) // WriteFile keeps the mode of a pre-existing file
}

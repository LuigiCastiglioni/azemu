package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"

	"github.com/luigi23997/azemu/internal/config"
	"github.com/luigi23997/azemu/internal/infra"
	"github.com/luigi23997/azemu/internal/settings"
)

// parseFlags allows flags after positional args: it re-parses what follows each positional.
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func loadModel(file, envFlag string, refresh bool) (*config.Config, *infra.Model, string, error) {
	cfg, err := config.Load(file)
	if err != nil {
		return nil, nil, "", err
	}
	env := envFlag
	if env == "" {
		env = cfg.Env
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	m, err := infra.Load(ctx, cfg, env, refresh)
	return cfg, m, env, err
}

func runEnv(args []string) error {
	fs := flag.NewFlagSet("env", flag.ContinueOnError)
	file := fs.String("f", ".", "azemu.yaml or its directory")
	envFlag := fs.String("env", "", "environment (default: env in azemu.yaml)")
	show := fs.Bool("show-secrets", false, "do not mask secret values")
	refresh := fs.Bool("refresh-infra", false, "force a new terraform plan")
	telemetry := fs.Bool("keep-telemetry", false, "keep App Insights settings")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: azemu env [-f azemu.yaml] [--env E] [--show-secrets] [--refresh-infra] [--keep-telemetry] <app>")
	}
	cfg, m, env, err := loadModel(*file, *envFlag, *refresh)
	if err != nil {
		return err
	}
	key := pos[0]
	if _, ok := cfg.Apps[key]; !ok { // allow the Azure resource name too
		for k := range cfg.Apps {
			if cfg.ResourceName(k, env) == key {
				key = k
			}
		}
	}
	if _, ok := cfg.Apps[key]; !ok {
		return fmt.Errorf("unknown app %q", pos[0])
	}
	out, err := settings.Resolve(cfg, m, key, env, settings.Options{IdentityHeader: "azemu-identity-header", KeepTelemetry: *telemetry})
	if err != nil {
		return err
	}
	raw := m.App(cfg.ResourceName(key, env)).AppSettings
	keys := make([]string, 0, len(out))
	for k := range out {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := out[k]
		if !*show && v != "" && (strings.Contains(raw[k], "@Microsoft.KeyVault(") || looksSecret(k, v)) {
			v = mask(v)
		}
		fmt.Printf("%s=%s\n", k, v)
	}
	for _, k := range m.App(cfg.ResourceName(key, env)).UnknownSettings {
		fmt.Fprintf(os.Stderr, "warning: setting %s is only known after apply; set it under apps.%s.env in azemu.yaml\n", k, key)
	}
	return nil
}

func looksSecret(k, v string) bool {
	u := strings.ToUpper(k)
	for _, s := range []string{"KEY", "SECRET", "TOKEN", "PASSWORD", "CONNECTION_STRING"} {
		if strings.Contains(u, s) {
			return true
		}
	}
	return strings.Contains(strings.ToLower(v), "accountkey=")
}

func mask(v string) string {
	r := []rune(v)
	if len(r) > 4 {
		r = r[:4]
	}
	return string(r) + "…"
}

func runSecrets(args []string) error {
	if len(args) == 0 || args[0] != "init" {
		return errors.New("usage: azemu secrets init [-f azemu.yaml] [--env E]")
	}
	fs := flag.NewFlagSet("secrets init", flag.ContinueOnError)
	file := fs.String("f", ".", "azemu.yaml or its directory")
	envFlag := fs.String("env", "", "environment (default: env in azemu.yaml)")
	if _, err := parseFlags(fs, args[1:]); err != nil {
		return err
	}
	cfg, m, env, err := loadModel(*file, *envFlag, false)
	if err != nil {
		return err
	}
	have, err := settings.ReadSecrets(cfg, env)
	if err != nil {
		return err
	}
	var add []string
	for _, n := range settings.SecretNames(m) {
		if _, ok := settings.Lookup(have, n); !ok {
			add = append(add, settings.Normalize(n))
		}
	}
	if len(add) == 0 {
		fmt.Println("nothing to add")
		return nil
	}
	path := cfg.Abs(config.Expand(cfg.Secrets, env))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	body := strings.Join(add, "=\n") + "=\n"
	if st, _ := f.Stat(); st.Size() > 0 { // the file may lack a trailing newline
		body = "\n" + body
	}
	if _, err := f.WriteString(body); err != nil {
		return err
	}
	fmt.Printf("added to %s: %s\n", path, strings.Join(add, " "))
	return nil
}

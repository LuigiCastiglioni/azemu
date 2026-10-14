package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/luigi23997/azemu/internal/config"
	"github.com/luigi23997/azemu/internal/discover"
	"github.com/luigi23997/azemu/internal/infra"
)

func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	env := fs.String("env", "dev", "environment to discover")
	infraPath := fs.String("infra", "", "infra repo or terraform directory")
	update := fs.Bool("update", false, "merge into an existing azemu.yaml")
	dry := fs.Bool("dry-run", false, "print the YAML instead of writing it")
	native := fs.Bool("native", false, "warn about docker apps whose run: for `up --native` cannot be derived (run: and host_header: are always written when derivable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	out := filepath.Join(cwd, config.FileName)
	_, statErr := os.Stat(out)
	exists := statErr == nil
	if exists && !*update && !*dry {
		return fmt.Errorf("%s already exists; use --update to merge or --dry-run to preview", config.FileName)
	}

	code, err := discover.Scan(cwd, *env)
	if err != nil {
		return err
	}
	tf, tfRoot := code.TF, cwd
	var warns []string
	warns = append(warns, code.Warnings...)
	if *infraPath != "" {
		root, tfDir := discover.ResolveInfra(*infraPath)
		ir, err := discover.Scan(root, *env)
		if err != nil {
			return err
		}
		warns = append(warns, ir.Warnings...)
		if ir.TF != nil {
			tf, tfRoot = ir.TF, root
		} else {
			tf, tfRoot = &discover.TF{Dir: "."}, root
		}
		if tfDir != "" {
			tf.Dir = rel(root, tfDir) // an explicit terraform dir wins over the pipeline's
		} else if _, err := os.Stat(filepath.Join(root, "terraform")); ir.TF == nil && err == nil {
			tf.Dir = "terraform"
		}
	}

	var inf config.Infra
	if tf != nil {
		inf = config.Infra{Dir: rel(cwd, filepath.Join(tfRoot, tf.Dir)), VarFiles: tf.VarFiles, BackendConfig: tf.BackendConfig}
	} else {
		warns = append(warns, "no terraform step found in the pipelines; pass --infra <infra repo or terraform dir>")
	}

	var model *infra.Model
	if inf.Dir != "" {
		cfg := &config.Config{Env: *env, Infra: inf, Root: cwd, Secrets: ".env", Apps: map[string]*config.App{}}
		if model, err = infra.Load(context.Background(), cfg, *env, false); err != nil {
			warns = append(warns, fmt.Sprintf("terraform model unavailable (%v); apps are written unmatched", err))
			model = nil
		}
	}

	draft := discover.Draft(cwd, *env, inf, discover.Match(code.Findings, model))
	for _, k := range draft.FillNative(cwd) {
		if *native { // run: is only needed by `up --native`; don't nag everyone else
			warns = append(warns, fmt.Sprintf("%s: could not derive `run:` for --native from its Dockerfile; add it under apps.%s", k, k))
		}
	}
	var b []byte
	if exists && *update {
		old, err := os.ReadFile(out)
		if err != nil {
			return err
		}
		b, err = draft.MergeInto(old, *env)
		if err != nil {
			return err
		}
	} else if b, err = draft.Render(); err != nil {
		return err
	}

	for _, w := range warns {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	if *dry {
		fmt.Print(string(b))
		return nil
	}
	if err := os.WriteFile(out, b, 0o644); err != nil {
		return err
	}
	if err := ensureGitignore(cwd, ".azemu/", ".env*"); err != nil {
		return err
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "APP\tRESOURCE\tKIND\tSOURCE\tCONFIDENCE")
	for _, r := range draft.Rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.Key, r.Resource, r.Kind, r.Source, r.Confidence)
	}
	tw.Flush()
	for _, t := range draft.Todos {
		fmt.Println(t)
	}
	fmt.Printf("\nwrote %s. Review it, then:\n  azemu secrets init\n  azemu up\n", config.FileName)
	return nil
}

func rel(base, p string) string {
	if r, err := filepath.Rel(base, p); err == nil {
		return filepath.ToSlash(r)
	}
	return filepath.ToSlash(p)
}

// ensureGitignore appends the missing lines to dir/.gitignore.
func ensureGitignore(dir string, lines ...string) error {
	p := filepath.Join(dir, ".gitignore")
	b, err := os.ReadFile(p)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	have := map[string]bool{}
	for _, l := range strings.Split(string(b), "\n") {
		have[strings.TrimSpace(l)] = true
	}
	var add string
	for _, l := range lines {
		if !have[l] {
			add += l + "\n"
		}
	}
	if add == "" {
		return nil
	}
	if len(b) > 0 && !strings.HasSuffix(string(b), "\n") {
		add = "\n" + add
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(add)
	return err
}

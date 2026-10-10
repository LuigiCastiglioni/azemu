package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/luigi23997/azemu/internal/config"
	"github.com/luigi23997/azemu/internal/infra"
)

const infraUsage = `usage: azemu infra show   [-f azemu.yaml] [--env E] [--json]
       azemu infra verify [-f azemu.yaml] [--env E] [--plan-file F]`

func runInfra(args []string) error {
	if len(args) == 0 {
		return errors.New(infraUsage)
	}
	switch args[0] {
	case "show":
		return infraShow(args[1:])
	case "verify":
		return infraVerify(args[1:])
	}
	return errors.New(infraUsage)
}

func infraShow(args []string) error {
	fs := flag.NewFlagSet("infra show", flag.ContinueOnError)
	file := fs.String("f", ".", "azemu.yaml or its directory")
	envFlag := fs.String("env", "", "environment (default: env in azemu.yaml)")
	asJSON := fs.Bool("json", false, "print the full model as JSON (not masked)")
	refresh := fs.Bool("refresh-infra", false, "force a new terraform plan (plan mode)")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	cfg, m, env, err := loadModel(*file, *envFlag, *refresh)
	if err != nil {
		return err
	}
	if *asJSON {
		fmt.Fprintln(os.Stderr, "note: the JSON contains app settings in clear, including secrets; do not share or commit it")
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(m)
	}

	mode := "static"
	if cfg.Infra.Mode == "plan" || cfg.Infra.PlanFile != "" {
		mode = "plan"
	}
	fmt.Printf("env %s, mode %s, dir %s\n", env, mode, cfg.Abs(cfg.Infra.Dir))

	fmt.Printf("\nApps (%d)\n", len(m.Apps))
	for _, a := range m.Apps {
		rt := a.Runtime
		if a.RuntimeVersion != "" {
			rt += " " + a.RuntimeVersion
		}
		if a.DockerImage != "" {
			rt = "image " + a.DockerImage
		}
		fmt.Printf("  %s\n    %s, %s, %s, %s", a.Name, a.Host, a.Kind, a.ResourceType, strings.TrimSpace(rt))
		if a.HostStorage != "" {
			mi := ""
			if a.HostStorageMI {
				mi = " (managed identity)"
			}
			fmt.Printf(", host storage %s%s", a.HostStorage, mi)
		}
		if a.EasyAuth {
			fmt.Print(", easy auth")
		}
		fmt.Printf("\n    %d settings", len(a.AppSettings))
		if len(a.UnknownSettings) > 0 {
			fmt.Printf(", unknown: %s", strings.Join(a.UnknownSettings, " "))
		}
		fmt.Println()
	}

	fmt.Printf("\nStorage accounts (%d)\n", len(m.StorageAccounts))
	for _, s := range m.StorageAccounts {
		fmt.Printf("  %s\n", s.Name)
		for _, kv := range []struct {
			k string
			v []string
		}{{"containers", s.Containers}, {"tables", s.Tables}, {"queues", s.Queues}} {
			if len(kv.v) > 0 {
				fmt.Printf("    %s: %s\n", kv.k, strings.Join(kv.v, " "))
			}
		}
	}

	fmt.Printf("\nKey vaults (%d)\n", len(m.Vaults))
	for _, v := range m.Vaults {
		fmt.Printf("  %s\n", v)
	}

	if len(m.Warnings) > 0 {
		fmt.Printf("\nWarnings (%d)\n", len(m.Warnings))
		for _, w := range m.Warnings {
			fmt.Printf("  %s\n", w)
		}
	}
	return nil
}

// infraVerify diffs the static model against terraform plan (the oracle) and fails on mismatches.
func infraVerify(args []string) error {
	fs := flag.NewFlagSet("infra verify", flag.ContinueOnError)
	file := fs.String("f", ".", "azemu.yaml or its directory")
	envFlag := fs.String("env", "", "environment (default: env in azemu.yaml)")
	planFile := fs.String("plan-file", "", "terraform show -json output or saved plan binary (default: infra.plan_file, else run terraform plan)")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	cfg, err := config.Load(*file)
	if err != nil {
		return err
	}
	env := *envFlag
	if env == "" {
		env = cfg.Env
	}

	staticCfg, planCfg := *cfg, *cfg
	staticCfg.Infra.Mode, staticCfg.Infra.PlanFile = "static", ""
	planCfg.Infra.Mode = "plan"
	if *planFile != "" {
		if planCfg.Infra.PlanFile, err = filepath.Abs(*planFile); err != nil {
			return err
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var static, plan *infra.Model
	var sErr, pErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); static, sErr = infra.Load(ctx, &staticCfg, env, false) }()
	go func() { defer wg.Done(); plan, pErr = infra.Load(ctx, &planCfg, env, false) }()
	wg.Wait()
	if sErr != nil {
		return fmt.Errorf("static model: %w", sErr)
	}
	if pErr != nil {
		return fmt.Errorf("plan model: %w", pErr)
	}

	res := infra.Diff(static, plan)
	fmt.Printf("static: %d apps, %d storage accounts, %d vaults\n", len(static.Apps), len(static.StorageAccounts), len(static.Vaults))
	fmt.Printf("plan:   %d apps, %d storage accounts, %d vaults\n", len(plan.Apps), len(plan.StorageAccounts), len(plan.Vaults))
	fmt.Printf("compared %d values, skipped %d (unknown in the plan until apply)\n", res.Compared, res.Skipped)
	if len(res.Mismatches) == 0 {
		fmt.Println("OK: the static model agrees with terraform plan")
		return nil
	}
	fmt.Printf("\n%d mismatches\n", len(res.Mismatches))
	for _, m := range res.Mismatches {
		if strings.HasPrefix(m.Field, "setting ") && looksSecret(strings.TrimPrefix(m.Field, "setting "), m.Static+m.Plan) {
			m.Static, m.Plan = maskIf(m.Static), maskIf(m.Plan)
		}
		fmt.Println("  " + m.String())
	}
	counts := map[string]int{}
	for _, m := range res.Mismatches {
		counts[m.Kind]++
	}
	var parts []string
	for k, n := range counts {
		parts = append(parts, fmt.Sprintf("%d %s", n, k))
	}
	sort.Strings(parts)
	return fmt.Errorf("static model differs from terraform plan: %s", strings.Join(parts, ", "))
}

func maskIf(v string) string {
	if v == "" {
		return v
	}
	return mask(v)
}

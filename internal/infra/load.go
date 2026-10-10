package infra

import (
	"context"
	"fmt"
	"os"

	"github.com/luigi23997/azemu/internal/config"
	"github.com/luigi23997/azemu/internal/infra/tfstatic"
)

// Load returns the infra model for env: static evaluation by default, terraform plan when
// infra.mode is "plan" or infra.plan_file is set.
func Load(ctx context.Context, cfg *config.Config, env string, refresh bool) (*Model, error) {
	if cfg.Infra.Mode == "plan" || cfg.Infra.PlanFile != "" {
		return Plan(ctx, cfg, env, refresh)
	}
	if cfg.Infra.Mode != "" && cfg.Infra.Mode != "static" {
		return nil, fmt.Errorf("infra.mode %q: want static or plan", cfg.Infra.Mode)
	}
	var vf []string
	for _, v := range cfg.Infra.VarFiles {
		vf = append(vf, config.Expand(v, env))
	}
	js, warns, err := tfstatic.Evaluate(ctx, tfstatic.Options{
		Dir:      cfg.Abs(cfg.Infra.Dir),
		VarFiles: vf,
		DataDir:  cfg.StatePath("tf", env),
	})
	if err != nil {
		return nil, err
	}
	m, err := FromPlanJSON(js, env)
	if err != nil {
		return nil, err
	}
	m.Warnings = append(warns, m.Warnings...)
	for _, w := range warns {
		fmt.Fprintln(os.Stderr, "[infra] "+w)
	}
	return m, nil
}

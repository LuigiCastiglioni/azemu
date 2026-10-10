// Package tfstatic evaluates Terraform configuration locally, without providers, state or
// Azure access, and emits a `terraform show -json`-shaped plan document for infra.FromPlanJSON.
package tfstatic

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

type Options struct {
	Dir      string   // terraform root module dir (absolute)
	VarFiles []string // relative to Dir, already env-expanded, applied in order
	DataDir  string   // azemu-owned TF_DATA_DIR for `terraform init -backend=false` (remote modules)
}

// Evaluate returns plan JSON (format_version, planned_values.root_module{resources,child_modules},
// resource_changes[].change.{after,after_unknown}) plus human-readable warnings.
//
// Terraform's own semantics for the subset azemu needs: variables (types, optional() defaults,
// tfvars, TF_VAR_*), locals, modules (local or remote, count/for_each), resources and data sources
// (count/for_each, dynamic blocks) and Terraform's function set. Attributes Azure assigns at apply
// are filled by Computed (naming conventions) or left unknown. Unevaluable parts degrade to
// unknown + a warning; only a missing/invalid root variable or a syntax error is an error.
func Evaluate(ctx context.Context, o Options) (planJSON []byte, warnings []string, err error) {
	defer func() { // cty panics on values it considers programmer errors; never take azemu down for a config
		if r := recover(); r != nil {
			planJSON, warnings, err = nil, nil, fmt.Errorf("tfstatic: internal error: %v", r)
		}
	}()
	dir, err := filepath.Abs(o.Dir)
	if err != nil {
		return nil, nil, err
	}
	e := &ev{o: o, dir: dir, funcs: funcs(dir), cfgs: map[string]*modCfg{}, cache: map[string]*entry{}, warns: map[string]bool{},
		mods: &moduleSources{ctx: ctx, root: dir, dataDir: o.DataDir}, computeFn: computed}
	root, err := loadModule(dir, e.warnPath)
	if err != nil {
		return nil, nil, err
	}
	e.cfgs[dir] = root
	if err := e.loadRootVars(root); err != nil {
		return nil, nil, err
	}
	e.walk(&inst{cfg: root})
	js, err := e.planJSON()
	if err != nil {
		return nil, nil, err
	}
	for w := range e.warns {
		warnings = append(warnings, w)
	}
	sort.Strings(warnings)
	return js, warnings, nil
}

// loadRootVars resolves root module variables with Terraform's precedence: TF_VAR_* < terraform.tfvars[.json]
// < *.auto.tfvars[.json] (lexical) < Options.VarFiles in order.
func (e *ev) loadRootVars(root *modCfg) error {
	supplied := map[string]cty.Value{}
	var bad []string
	for _, kv := range os.Environ() {
		k, val, _ := strings.Cut(kv, "=")
		n, ok := strings.CutPrefix(k, "TF_VAR_")
		vc := root.vars[n]
		if !ok || vc == nil {
			continue
		}
		if vc.typ == cty.String || vc.typ == cty.DynamicPseudoType {
			supplied[n] = cty.StringVal(val)
			continue
		}
		ex, d := hclsyntax.ParseExpression([]byte(val), k, hcl.InitialPos)
		if v, d2 := ex.Value(nil); !d.HasErrors() && !d2.HasErrors() {
			supplied[n] = v
		} else {
			bad = append(bad, k+": not a valid HCL value")
		}
	}

	ents, _ := os.ReadDir(e.dir)
	var files, auto []string
	for _, ent := range ents {
		switch n := ent.Name(); {
		case n == "terraform.tfvars":
			files = append([]string{n}, files...)
		case n == "terraform.tfvars.json":
			files = append(files, n)
		case strings.HasSuffix(n, ".auto.tfvars") || strings.HasSuffix(n, ".auto.tfvars.json"):
			auto = append(auto, n)
		}
	}
	files = append(append(files, auto...), e.o.VarFiles...)
	for _, f := range files {
		if !filepath.IsAbs(f) {
			f = filepath.Join(e.dir, f)
		}
		vals, err := readVarFile(f)
		if err != nil {
			return err
		}
		for k, v := range vals {
			if root.vars[k] != nil {
				supplied[k] = v
			}
		}
	}

	e.rootVars = map[string]cty.Value{}
	var missing []string
	for _, n := range sortedKeys(root.vars) {
		vc := root.vars[n]
		v, ok := supplied[n]
		if !ok && !vc.hasDef {
			missing = append(missing, n)
			continue
		}
		if !ok || (v.IsNull() && !vc.nullable && vc.hasDef) {
			v = vc.def
		}
		v, err := vc.coerce(v)
		if err != nil {
			bad = append(bad, fmt.Sprintf("variable %s: %v", n, err))
			continue
		}
		e.rootVars[n] = v
	}
	var msgs []string
	if len(missing) > 0 {
		msgs = append(msgs, "missing required variables: "+strings.Join(missing, ", "))
	}
	if len(bad) > 0 {
		msgs = append(msgs, "invalid variable values: "+strings.Join(bad, "; "))
	}
	if len(msgs) > 0 {
		return fmt.Errorf("%s", strings.Join(msgs, "; "))
	}
	return nil
}

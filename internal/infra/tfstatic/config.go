package tfstatic

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/ext/typeexpr"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

// modCfg is one parsed module directory (shared by all its instances).
type modCfg struct {
	dir     string
	vars    map[string]*varCfg
	locals  map[string]*hclsyntax.Attribute
	outputs map[string]*hclsyntax.Attribute // name -> value attribute
	calls   map[string]*callCfg
	res     map[string]*resCfg // "type.name" (managed) or "data.type.name"
	types   map[string]bool    // managed resource types present (reference classification)
}

type varCfg struct {
	typ      cty.Type
	defs     *typeexpr.Defaults
	def      cty.Value
	hasDef   bool
	nullable bool
}

type callCfg struct {
	name   string
	source string
	body   *hclsyntax.Body
	rng    hcl.Range
}

type resCfg struct {
	data      bool
	typ, name string
	body      *hclsyntax.Body
	rng       hcl.Range
}

func (r *resCfg) key() string {
	if r.data {
		return "data." + r.typ + "." + r.name
	}
	return r.typ + "." + r.name
}

// loadModule parses every *.tf in dir. Override files are ignored (warned).
func loadModule(dir string, warn func(path, msg string)) (*modCfg, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	m := &modCfg{dir: dir, vars: map[string]*varCfg{}, locals: map[string]*hclsyntax.Attribute{}, outputs: map[string]*hclsyntax.Attribute{},
		calls: map[string]*callCfg{}, res: map[string]*resCfg{}, types: map[string]bool{}}
	p := hclparse.NewParser()
	for _, ent := range ents {
		n := ent.Name()
		switch {
		case ent.IsDir() || strings.HasPrefix(n, ".") || strings.HasSuffix(n, "~"):
			continue
		case n == "override.tf" || strings.HasSuffix(n, "_override.tf"):
			warn(filepath.Join(dir, n), "override file ignored")
			continue
		case strings.HasSuffix(n, ".tf.json"):
			warn(filepath.Join(dir, n), ".tf.json is not supported, ignored")
			continue
		case !strings.HasSuffix(n, ".tf"):
			continue
		}
		f, diags := p.ParseHCLFile(filepath.Join(dir, n))
		if diags.HasErrors() {
			return nil, fmt.Errorf("%s", diags.Error())
		}
		body, ok := f.Body.(*hclsyntax.Body)
		if !ok {
			continue
		}
		for _, b := range body.Blocks {
			if err := m.add(b); err != nil {
				return nil, err
			}
		}
	}
	return m, nil
}

func (m *modCfg) add(b *hclsyntax.Block) error {
	label := func(i int) string {
		if i < len(b.Labels) {
			return b.Labels[i]
		}
		return ""
	}
	switch b.Type {
	case "variable":
		vc := &varCfg{typ: cty.DynamicPseudoType, nullable: true}
		if a := b.Body.Attributes["type"]; a != nil {
			t, d, diags := typeexpr.TypeConstraintWithDefaults(a.Expr)
			if diags.HasErrors() {
				return fmt.Errorf("variable %q: %s", label(0), diags.Error())
			}
			vc.typ, vc.defs = t, d
		}
		if a := b.Body.Attributes["default"]; a != nil {
			v, diags := a.Expr.Value(nil)
			if diags.HasErrors() {
				return fmt.Errorf("variable %q default: %s", label(0), diags.Error())
			}
			vc.def, vc.hasDef = v, true
		}
		if a := b.Body.Attributes["nullable"]; a != nil {
			if v, diags := a.Expr.Value(nil); !diags.HasErrors() && v.Type() == cty.Bool && v.IsKnown() && !v.IsNull() {
				vc.nullable = v.True()
			}
		}
		m.vars[label(0)] = vc
	case "locals":
		for n, a := range b.Body.Attributes {
			m.locals[n] = a
		}
	case "output":
		if a := b.Body.Attributes["value"]; a != nil {
			m.outputs[label(0)] = a
		}
	case "module":
		c := &callCfg{name: label(0), body: b.Body, rng: b.DefRange()}
		if a := b.Body.Attributes["source"]; a != nil {
			if v, d := a.Expr.Value(nil); !d.HasErrors() && v.Type() == cty.String && v.IsKnown() && !v.IsNull() {
				c.source = v.AsString()
			}
		}
		m.calls[c.name] = c
	case "resource", "data":
		r := &resCfg{data: b.Type == "data", typ: label(0), name: label(1), body: b.Body, rng: b.DefRange()}
		m.res[r.key()] = r
		if !r.data {
			m.types[r.typ] = true
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// readVarFile reads a .tfvars (HCL) or .tfvars.json file.
func readVarFile(path string) (map[string]cty.Value, error) {
	out := map[string]cty.Value{}
	if strings.HasSuffix(path, ".json") {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(b, &raw); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		for k, r := range raw {
			t, err := ctyjson.ImpliedType(r)
			if err != nil {
				return nil, fmt.Errorf("%s: %s: %w", path, k, err)
			}
			if out[k], err = ctyjson.Unmarshal(r, t); err != nil {
				return nil, fmt.Errorf("%s: %s: %w", path, k, err)
			}
		}
		return out, nil
	}
	f, diags := hclparse.NewParser().ParseHCLFile(path)
	if diags.HasErrors() {
		return nil, fmt.Errorf("%s", diags.Error())
	}
	attrs, diags := f.Body.JustAttributes()
	if diags.HasErrors() {
		return nil, fmt.Errorf("%s", diags.Error())
	}
	for k, a := range attrs {
		v, d := a.Expr.Value(nil)
		if d.HasErrors() {
			return nil, fmt.Errorf("%s: variable %s: %s", path, k, d.Error())
		}
		out[k] = v
	}
	return out, nil
}

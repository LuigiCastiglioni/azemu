package tfstatic

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
	"github.com/zclconf/go-cty/cty/function"
)

// computed is swapped by tests.
var computed = Computed

// ev evaluates a module tree lazily: every local, variable, output, resource and module call is a
// memoized node, computed the first time an expression references it. A node that (transitively)
// references itself yields an unknown value plus a warning instead of recursing forever.
type ev struct {
	o         Options
	dir       string // root dir (absolute); file functions and path.cwd
	funcs     map[string]function.Function
	cfgs      map[string]*modCfg
	cache     map[string]*entry
	warns     map[string]bool
	rootVars  map[string]cty.Value
	insts     []*inst // every module instance discovered, root first
	mods      *moduleSources
	computeFn func(typ string, cfg cty.Value, attr string) (cty.Value, bool)
	buf       *[]string // warnings of the evaluation attempt in progress (see retry)
}

type entry struct {
	v    any
	busy bool
}

// memo computes f once per key; zero is returned (with a warning) when key is requested while f runs.
func memo[T any](e *ev, key string, zero T, f func() T) T {
	if c, ok := e.cache[key]; ok {
		if c.busy {
			e.warn("dependency cycle at " + strings.TrimPrefix(key, "|"))
			return zero
		}
		return c.v.(T)
	}
	c := &entry{busy: true}
	e.cache[key] = c
	saved := e.buf // a node's warnings belong to the node, not to whoever asked first
	e.buf = nil
	v := f()
	e.buf = saved
	c.v, c.busy = v, false
	return v
}

func (e *ev) warn(msg string) {
	if e.buf != nil {
		*e.buf = append(*e.buf, msg)
	} else {
		e.warns[msg] = true
	}
}

func (e *ev) warnPath(path, msg string) { e.warn(e.rel(path) + ": " + msg) }

func (e *ev) warnAt(r hcl.Range, format string, a ...any) {
	e.warn(fmt.Sprintf("%s:%d: %s", e.rel(r.Filename), r.Start.Line, fmt.Sprintf(format, a...)))
}

func (e *ev) warnDiags(d hcl.Diagnostics, what string) {
	for _, x := range d {
		if x.Severity != hcl.DiagError {
			continue
		}
		msg := x.Summary
		if x.Detail != "" {
			msg += ": " + strings.SplitN(x.Detail, "\n", 2)[0]
		}
		if x.Subject != nil {
			e.warnAt(*x.Subject, "%s unknown (%s)", what, msg)
		} else {
			e.warn(fmt.Sprintf("%s unknown (%s)", what, msg))
		}
	}
}

// rel prints p relative to the root module dir (remote modules: relative to the data dir).
func (e *ev) rel(p string) string {
	for _, base := range []string{e.o.DataDir, e.dir} {
		if base == "" {
			continue
		}
		if r, err := filepath.Rel(base, p); err == nil && (base == e.dir || !strings.HasPrefix(r, "..")) {
			return r
		}
	}
	return p
}

// inst is one instance of a module (the root, or one expansion of a module call).
type inst struct {
	cfg    *modCfg
	addr   string // "" for root, else module.a["k"].module.b[0]
	path   string // module names without instance keys: "a.b" (modules.json key)
	parent *inst
	call   *callCfg
	extra  map[string]cty.Value // count/each visible to the call arguments (evaluated in parent)
}

func join(prefix, s string) string {
	if prefix == "" {
		return s
	}
	return prefix + "." + s
}

type mode int

const (
	single mode = iota
	counted
	keyed
)

type item struct {
	key    cty.Value
	suffix string
	extra  map[string]cty.Value
}

type expansion struct {
	mode    mode
	items   []item
	skipped bool // for_each unknown: nothing to instantiate
}

// expand evaluates count / for_each of a resource or module call.
func (e *ev) expand(m *inst, b *hclsyntax.Body, what string) expansion {
	if a := b.Attributes["count"]; a != nil {
		n := 1
		v, err := convert.Convert(e.value(m, a.Expr, nil, what+" count"), cty.Number)
		if err == nil && v.IsKnown() && !v.IsNull() {
			i, _ := v.AsBigFloat().Int64()
			n = max(int(i), 0)
		} else {
			e.warnAt(a.Range(), "%s: count unknown until apply, assuming 1", what)
		}
		x := expansion{mode: counted}
		for i := range n {
			x.items = append(x.items, item{key: cty.NumberIntVal(int64(i)), suffix: fmt.Sprintf("[%d]", i),
				extra: map[string]cty.Value{"count": cty.ObjectVal(map[string]cty.Value{"index": cty.NumberIntVal(int64(i))})}})
		}
		return x
	}
	if a := b.Attributes["for_each"]; a != nil {
		v := e.value(m, a.Expr, nil, what+" for_each")
		t := v.Type()
		if !v.IsKnown() || v.IsNull() || (t.IsSetType() && !v.IsWhollyKnown()) || !(t.IsMapType() || t.IsObjectType() || t.IsSetType()) {
			e.warnAt(a.Range(), "%s: for_each unknown or not a map/set, skipped", what)
			return expansion{mode: keyed, skipped: true}
		}
		x := expansion{mode: keyed}
		for it := v.ElementIterator(); it.Next(); {
			k, el := it.Element()
			if t.IsSetType() {
				if el.Type() != cty.String {
					e.warnAt(a.Range(), "%s: for_each set must contain strings, skipped", what)
					return expansion{mode: keyed, skipped: true}
				}
				k = el
			}
			x.items = append(x.items, item{key: k, suffix: fmt.Sprintf("[%q]", k.AsString()),
				extra: map[string]cty.Value{"each": cty.ObjectVal(map[string]cty.Value{"key": k, "value": el})}})
		}
		return x
	}
	return expansion{items: []item{{}}}
}

// ---- references ----

type need struct {
	all   bool
	attrs map[string]bool
	deep  map[string]bool // attrs followed by an index/attribute step: not a plain string
}

func (n *need) add(attr string, deep bool) {
	if attr == "" {
		n.all = true
		return
	}
	n.attrs[attr] = true
	if deep {
		n.deep[attr] = true
	}
}

// refs lists what an expression (or whole body) references, with the attribute asked of each
// module/resource so that only the needed outputs / computed attributes are evaluated.
type refs struct {
	vars, locals map[string]bool
	mods         map[string]*need
	data, res    map[string]*need // "type.name"
	dirty        bool             // learn() added something: the evaluation must be redone
}

var unsupportedAttr = regexp.MustCompile(`named "([^"]+)"`)

// learn adapts the references after a failed evaluation, so that the next attempt can succeed:
//   - `for v in azurerm_x.y : v.attr`: the resource is referenced as a whole, so the attributes
//     asked of its elements are only known from the "unsupported attribute" error;
//   - unknown attributes are guessed to be strings (see attr); if something failed, assume the
//     guess was wrong and use untyped unknowns instead.
func (r *refs) learn(d hcl.Diagnostics) {
	for _, x := range d {
		if x.Summary != "Unsupported attribute" {
			continue
		}
		if mm := unsupportedAttr.FindStringSubmatch(x.Detail); mm != nil {
			for _, set := range []map[string]*need{r.res, r.data} {
				for _, n := range set {
					if n.all && !n.attrs[mm[1]] {
						n.add(mm[1], true)
						r.dirty = true
					}
				}
			}
		}
	}
	for _, set := range []map[string]*need{r.res, r.data} {
		for _, n := range set {
			for a := range n.attrs {
				if !n.deep[a] {
					n.deep[a] = true
					r.dirty = true
				}
			}
		}
	}
}

func (r *refs) needOf(m map[string]*need, k string) *need {
	if m[k] == nil {
		m[k] = &need{attrs: map[string]bool{}, deep: map[string]bool{}}
	}
	return m[k]
}

func collect(cfg *modCfg, n hclsyntax.Node) *refs {
	r := &refs{vars: map[string]bool{}, locals: map[string]bool{}, mods: map[string]*need{}, data: map[string]*need{}, res: map[string]*need{}}
	handled := map[*hclsyntax.ScopeTraversalExpr]bool{}
	// attr of the first attribute step after position i (indexes skipped), else extra; deep when more steps follow.
	attrAt := func(t hcl.Traversal, i int, extra string, extraDeep bool) (string, bool) {
		for ; i < len(t); i++ {
			switch s := t[i].(type) {
			case hcl.TraverseIndex:
			case hcl.TraverseAttr:
				return s.Name, i+1 < len(t)
			default:
				return "", false
			}
		}
		return extra, extraDeep
	}
	name := func(t hcl.Traversal, i int) string {
		if i < len(t) {
			if a, ok := t[i].(hcl.TraverseAttr); ok {
				return a.Name
			}
		}
		return ""
	}
	trav := func(t hcl.Traversal, extra string, extraDeep bool) {
		switch root := t.RootName(); root {
		case "var":
			r.vars[name(t, 1)] = true
		case "local":
			r.locals[name(t, 1)] = true
		case "module":
			r.needOf(r.mods, name(t, 1)).add(attrAt(t, 2, extra, extraDeep))
		case "data":
			r.needOf(r.data, name(t, 1)+"."+name(t, 2)).add(attrAt(t, 3, extra, extraDeep))
		default:
			if k := root + "." + name(t, 1); cfg.types[root] && cfg.res[k] != nil {
				r.needOf(r.res, k).add(attrAt(t, 2, extra, extraDeep))
			}
		}
	}
	// `module.x[expr].attr` and `module.x[*].attr` hide the attr outside the scope traversal.
	hclsyntax.VisitAll(n, func(n hclsyntax.Node) hcl.Diagnostics {
		switch n := n.(type) {
		case *hclsyntax.RelativeTraversalExpr:
			if ix, ok := n.Source.(*hclsyntax.IndexExpr); ok {
				if st, ok := ix.Collection.(*hclsyntax.ScopeTraversalExpr); ok && !handled[st] {
					handled[st] = true
					trav(st.Traversal, name(n.Traversal, 0), len(n.Traversal) > 1)
				}
			}
		case *hclsyntax.SplatExpr:
			if st, ok := n.Source.(*hclsyntax.ScopeTraversalExpr); ok && !handled[st] {
				if rt, ok := n.Each.(*hclsyntax.RelativeTraversalExpr); ok {
					handled[st] = true
					trav(st.Traversal, name(rt.Traversal, 0), len(rt.Traversal) > 1)
				}
			}
		case *hclsyntax.ScopeTraversalExpr:
			if !handled[n] {
				trav(n.Traversal, "", false)
			}
		}
		return nil
	})
	return r
}

// context builds the EvalContext for evaluating something in module instance m.
func (e *ev) context(m *inst, r *refs, extra map[string]cty.Value) *hcl.EvalContext {
	rel := func(p string) string {
		if x, err := filepath.Rel(e.dir, p); err == nil {
			return x
		}
		return p
	}
	vars := map[string]cty.Value{
		"path":      cty.ObjectVal(map[string]cty.Value{"module": cty.StringVal(rel(m.cfg.dir)), "root": cty.StringVal("."), "cwd": cty.StringVal(e.dir)}),
		"terraform": cty.ObjectVal(map[string]cty.Value{"workspace": cty.StringVal("default")}),
	}
	vs, ls, ms := map[string]cty.Value{}, map[string]cty.Value{}, map[string]cty.Value{}
	for _, n := range sortedKeys(r.vars) {
		if m.cfg.vars[n] != nil {
			vs[n] = e.varVal(m, n)
		}
	}
	for _, n := range sortedKeys(r.locals) {
		if a := m.cfg.locals[n]; a != nil {
			ls[n] = memo(e, m.addr+"|local|"+n, cty.DynamicVal, func() cty.Value { return e.value(m, a.Expr, nil, "local."+n) })
		}
	}
	for _, n := range sortedKeys(r.mods) {
		if c := m.cfg.calls[n]; c != nil {
			ms[n] = e.modValue(m, c, r.mods[n])
		}
	}
	vars["var"], vars["local"], vars["module"] = cty.ObjectVal(vs), cty.ObjectVal(ls), cty.ObjectVal(ms)

	byType := map[string]map[string]cty.Value{}
	put := func(typ, name string, v cty.Value) {
		if byType[typ] == nil {
			byType[typ] = map[string]cty.Value{}
		}
		byType[typ][name] = v
	}
	for _, k := range sortedKeys(r.res) {
		typ, name, _ := strings.Cut(k, ".")
		put(typ, name, e.resValue(m, m.cfg.res[k], r.res[k]))
	}
	data := map[string]map[string]cty.Value{}
	for _, k := range sortedKeys(r.data) {
		if rc := m.cfg.res["data."+k]; rc != nil {
			typ, name, _ := strings.Cut(k, ".")
			if data[typ] == nil {
				data[typ] = map[string]cty.Value{}
			}
			data[typ][name] = e.resValue(m, rc, r.data[k])
		}
	}
	dv := map[string]cty.Value{}
	for t, mm := range data {
		dv[t] = cty.ObjectVal(mm)
	}
	vars["data"] = cty.ObjectVal(dv)
	for t, mm := range byType {
		vars[t] = cty.ObjectVal(mm)
	}
	for k, v := range extra {
		vars[k] = v
	}
	return &hcl.EvalContext{Variables: vars, Functions: e.funcs}
}

// value evaluates one expression; errors become an unknown value plus a warning.
func (e *ev) value(m *inst, ex hclsyntax.Expression, extra map[string]cty.Value, what string) cty.Value {
	r := collect(m.cfg, ex)
	var out cty.Value
	e.retry(r, func() {
		v, d := ex.Value(e.context(m, r, extra))
		out = e.partial(r, v, d, what)
	})
	return out
}

// retry runs f until it stops learning new attributes (refs.learn); only the last run's warnings count.
func (e *ev) retry(r *refs, f func()) {
	for i := 0; ; i++ {
		var buf []string
		saved := e.buf
		e.buf, r.dirty = &buf, false
		f()
		e.buf = saved
		if !r.dirty || i == 8 {
			for _, w := range buf {
				e.warn(w)
			}
			return
		}
	}
}

// partial returns hcl's best-effort result of a failed evaluation: parts that failed are already
// unknown placeholders (a merge() with one bad entry keeps the others), so it is safe to keep.
func (e *ev) partial(r *refs, v cty.Value, d hcl.Diagnostics, what string) cty.Value {
	if d.HasErrors() {
		r.learn(d)
		e.warnDiags(d, what)
		if v.Type() == cty.NilType {
			return cty.DynamicVal
		}
	}
	return v
}

// ---- variables ----

func (vc *varCfg) coerce(v cty.Value) (cty.Value, error) {
	if vc.defs != nil {
		v = vc.defs.Apply(v)
	}
	return convert.Convert(v, vc.typ)
}

func (e *ev) varVal(m *inst, name string) cty.Value {
	if m.parent == nil {
		return e.rootVars[name]
	}
	vc := m.cfg.vars[name]
	return memo(e, m.addr+"|var|"+name, cty.DynamicVal, func() cty.Value {
		what := fmt.Sprintf("%s var.%s", m.addr, name)
		v, set := cty.NilVal, false
		if a := m.call.body.Attributes[name]; a != nil {
			v, set = e.value(m.parent, a.Expr, m.extra, what), true
		}
		if (!set || (v.IsNull() && !vc.nullable)) && vc.hasDef {
			v, set = vc.def, true
		}
		if !set {
			e.warnAt(m.call.rng, "%s: required argument not set", what)
			return cty.UnknownVal(vc.typ)
		}
		v, err := vc.coerce(v)
		if err != nil {
			e.warn(fmt.Sprintf("%s: %v", what, err))
			return cty.UnknownVal(vc.typ)
		}
		return v
	})
}

// ---- module calls ----

type kidSet struct {
	expansion
	insts []*inst
}

func (e *ev) kids(m *inst, c *callCfg) *kidSet {
	skipped := &kidSet{expansion: expansion{skipped: true}}
	return memo(e, m.addr+"|call|"+c.name, skipped, func() *kidSet {
		if strings.Count(m.path, ".") >= 16 { // a module that calls itself
			e.warnAt(c.rng, "module.%s skipped: modules nested too deeply", c.name)
			return skipped
		}
		cfg, err := e.loadChild(m, c)
		if err != nil {
			e.warnAt(c.rng, "module.%s skipped: %v", c.name, err)
			return skipped
		}
		ks := &kidSet{expansion: e.expand(m, c.body, "module."+c.name)}
		for _, it := range ks.items {
			ks.insts = append(ks.insts, &inst{cfg: cfg, addr: join(m.addr, "module."+c.name+it.suffix), path: join(m.path, c.name), parent: m, call: c, extra: it.extra})
		}
		return ks
	})
}

func (e *ev) loadChild(m *inst, c *callCfg) (*modCfg, error) {
	if c.source == "" {
		return nil, fmt.Errorf("source is not a literal string")
	}
	dir := ""
	if strings.HasPrefix(c.source, "./") || strings.HasPrefix(c.source, "../") || filepath.IsAbs(c.source) {
		dir = c.source
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(m.cfg.dir, dir)
		}
	} else {
		var err error
		if dir, err = e.mods.dir(join(m.path, c.name), c.source); err != nil {
			return nil, err
		}
	}
	if cfg := e.cfgs[dir]; cfg != nil {
		return cfg, nil
	}
	cfg, err := loadModule(dir, e.warnPath)
	if err != nil {
		return nil, err
	}
	e.cfgs[dir] = cfg
	return cfg, nil
}

// modValue is the value of `module.<name>`: only the outputs the expression asks for are evaluated.
func (e *ev) modValue(m *inst, c *callCfg, n *need) cty.Value {
	ks := e.kids(m, c)
	if ks.skipped {
		return cty.DynamicVal
	}
	vals := make([]cty.Value, len(ks.insts))
	for i, k := range ks.insts {
		out := map[string]cty.Value{}
		for _, name := range sortedKeys(k.cfg.outputs) {
			if a := k.cfg.outputs[name]; n.all || n.attrs[name] {
				out[name] = memo(e, k.addr+"|out|"+name, cty.DynamicVal, func() cty.Value { return e.value(k, a.Expr, nil, k.addr+" output."+name) })
			}
		}
		vals[i] = cty.ObjectVal(out)
	}
	return e.wrap(ks.expansion, vals)
}

func (e *ev) wrap(x expansion, vals []cty.Value) cty.Value {
	switch x.mode {
	case counted:
		return cty.TupleVal(vals)
	case keyed:
		m := map[string]cty.Value{}
		for i, it := range x.items {
			m[it.key.AsString()] = vals[i]
		}
		return cty.ObjectVal(m)
	}
	return vals[0]
}

// ---- resources and data sources ----

type resInst struct {
	rc    *resCfg
	addr  string
	item  item
	attrs map[string]cty.Value // evaluated configuration
	extra map[string]cty.Value // attributes assigned by Azure, filled on demand
	// blocks already completed by Computed
	overlaid map[string]bool
}

func (ri *resInst) typ() string {
	if ri.rc.data {
		return "data." + ri.rc.typ
	}
	return ri.rc.typ
}

type resSet struct {
	expansion
	insts []*resInst
}

// meta-arguments and blocks that are not part of the resource's attributes.
var (
	metaAttrs  = map[string]bool{"count": true, "for_each": true, "depends_on": true, "provider": true}
	metaBlocks = map[string]bool{"lifecycle": true, "provisioner": true, "connection": true}
)

func (e *ev) resSet(m *inst, rc *resCfg) *resSet {
	skipped := &resSet{expansion: expansion{skipped: true}}
	return memo(e, m.addr+"|res|"+rc.key(), skipped, func() *resSet {
		x := e.expand(m, rc.body, join(m.addr, rc.key()))
		rs := &resSet{expansion: x}
		r := collect(m.cfg, rc.body)
		for _, it := range x.items {
			ri := &resInst{rc: rc, item: it, addr: join(m.addr, rc.key()+it.suffix), extra: map[string]cty.Value{}, overlaid: map[string]bool{}}
			e.retry(r, func() { ri.attrs = e.body(e.context(m, r, it.extra), r, rc.body, true, ri.addr) })
			rs.insts = append(rs.insts, ri)
		}
		return rs
	})
}

// body evaluates attributes and nested blocks (blocks become tuples of objects; dynamic blocks are expanded).
func (e *ev) body(ctx *hcl.EvalContext, r *refs, b *hclsyntax.Body, top bool, what string) map[string]cty.Value {
	out := map[string]cty.Value{}
	for _, name := range sortedKeys(b.Attributes) {
		if top && metaAttrs[name] {
			continue
		}
		v, d := b.Attributes[name].Expr.Value(ctx)
		out[name] = e.partial(r, v, d, what+"."+name)
	}
	blocks, unknown := map[string][]cty.Value{}, map[string]bool{}
	for _, blk := range b.Blocks {
		switch {
		case top && metaBlocks[blk.Type]:
		case blk.Type == "dynamic" && len(blk.Labels) == 1:
			e.dynamic(ctx, r, blk, blocks, unknown, what)
		default:
			blocks[blk.Type] = append(blocks[blk.Type], cty.ObjectVal(e.body(ctx, r, blk.Body, false, what+"."+blk.Type)))
		}
	}
	for t, l := range blocks {
		out[t] = cty.TupleVal(l)
	}
	for t := range unknown {
		out[t] = cty.DynamicVal
	}
	return out
}

func (e *ev) dynamic(ctx *hcl.EvalContext, r *refs, blk *hclsyntax.Block, blocks map[string][]cty.Value, unknown map[string]bool, what string) {
	name := blk.Labels[0]
	var content *hclsyntax.Block
	for _, c := range blk.Body.Blocks {
		if c.Type == "content" {
			content = c
		}
	}
	fe := blk.Body.Attributes["for_each"]
	if fe == nil || content == nil {
		e.warnAt(blk.Range(), "%s: dynamic %q needs for_each and content", what, name)
		unknown[name] = true
		return
	}
	iter := name
	if a := blk.Body.Attributes["iterator"]; a != nil {
		if t, d := hcl.AbsTraversalForExpr(a.Expr); !d.HasErrors() && len(t) == 1 {
			iter = t.RootName()
		}
	}
	coll, d := fe.Expr.Value(ctx)
	if d.HasErrors() {
		r.learn(d)
		e.warnDiags(d, what+".dynamic."+name)
		unknown[name] = true
		return
	}
	if !coll.IsKnown() || (coll.Type().IsSetType() && !coll.IsWhollyKnown()) {
		unknown[name] = true // data source output not known until apply
		return
	}
	if coll.IsNull() || !coll.CanIterateElements() {
		return
	}
	for it := coll.ElementIterator(); it.Next(); {
		k, v := it.Element()
		child := ctx.NewChild()
		child.Variables = map[string]cty.Value{iter: cty.ObjectVal(map[string]cty.Value{"key": k, "value": v})}
		blocks[name] = append(blocks[name], cty.ObjectVal(e.body(child, r, content.Body, false, what+"."+name)))
	}
}

// attr returns attribute name of a resource instance: configured, else assigned by Azure via
// computed (the naming conventions), else unknown.
func (e *ev) attr(ri *resInst, name string, deep bool) cty.Value {
	if v, ok := ri.attrs[name]; ok {
		return v
	}
	cfg := cty.ObjectVal(ri.attrs)
	v, ok := cty.NilVal, false
	if ri.typ() == "azurecaf_name" && name == "result" {
		v, ok = azurecafName(cfg)
	} else {
		v, ok = e.computeFn(ri.typ(), cfg, name)
	}
	if !ok {
		// Azure-assigned attributes are overwhelmingly strings; a typed unknown keeps merge()/conditionals
		// of settings maps from collapsing into one unknown map. Anything indexed further stays untyped.
		v = cty.UnknownVal(cty.String)
		if deep {
			v = cty.DynamicVal
		}
	}
	ri.extra[name] = v
	return v
}

// object is the instance value: configuration plus id, name and the attributes asked for. Blocks
// that are asked for (or all of them when the whole resource is referenced) are completed with what
// Azure assigns inside them, e.g. identity[0].principal_id.
func (e *ev) object(ri *resInst, n *need) cty.Value {
	for _, k := range sortedKeys(ri.attrs) {
		if v := ri.attrs[k]; (n.all || n.attrs[k]) && !ri.overlaid[k] && isBlocks(v) {
			ri.overlaid[k] = true
			if c, ok := e.computeFn(ri.typ(), cty.ObjectVal(ri.attrs), k); ok {
				ri.attrs[k] = overlay(v, c)
			}
		}
	}
	o := make(map[string]cty.Value, len(ri.attrs)+len(n.attrs)+2)
	for k, v := range ri.attrs {
		o[k] = v
	}
	for _, k := range append([]string{"id", "name"}, sortedKeys(n.attrs)...) {
		if _, ok := o[k]; !ok {
			o[k] = e.attr(ri, k, n.deep[k])
		}
	}
	return cty.ObjectVal(o)
}

func isBlocks(v cty.Value) bool {
	t := v.Type()
	if !v.IsKnown() || v.IsNull() || !t.IsTupleType() || len(t.TupleElementTypes()) == 0 {
		return false
	}
	for _, et := range t.TupleElementTypes() {
		if !et.IsObjectType() {
			return false
		}
	}
	return true
}

// overlay adds to each configured block the attributes comp (the conventional value) has and the
// configuration does not.
func overlay(cfg, comp cty.Value) cty.Value {
	if !comp.IsKnown() || comp.IsNull() || !comp.CanIterateElements() {
		return cfg
	}
	var cs []cty.Value
	for it := comp.ElementIterator(); it.Next(); {
		_, el := it.Element()
		cs = append(cs, el)
	}
	var out []cty.Value
	for i, el := range cfg.AsValueSlice() {
		if i < len(cs) && cs[i].IsKnown() && !cs[i].IsNull() && cs[i].Type().IsObjectType() {
			m := map[string]cty.Value{}
			for k, v := range cs[i].AsValueMap() {
				m[k] = v
			}
			for k, v := range el.AsValueMap() {
				m[k] = v
			}
			el = cty.ObjectVal(m)
		}
		out = append(out, el)
	}
	return cty.TupleVal(out)
}

func (e *ev) resValue(m *inst, rc *resCfg, n *need) cty.Value {
	rs := e.resSet(m, rc)
	if rs.skipped {
		return cty.DynamicVal
	}
	vals := make([]cty.Value, len(rs.insts))
	for i, ri := range rs.insts {
		vals[i] = e.object(ri, n)
	}
	return e.wrap(rs.expansion, vals)
}

// walk instantiates the whole tree: every azurerm_ resource of every module instance is evaluated
// (they are what gets emitted); everything else is only evaluated when referenced.
func (e *ev) walk(m *inst) {
	e.insts = append(e.insts, m)
	for _, k := range sortedKeys(m.cfg.res) {
		if rc := m.cfg.res[k]; !rc.data && strings.HasPrefix(rc.typ, "azurerm_") {
			e.resSet(m, rc)
		}
	}
	for _, name := range sortedKeys(m.cfg.calls) {
		for _, k := range e.kids(m, m.cfg.calls[name]).insts {
			e.walk(k)
		}
	}
}

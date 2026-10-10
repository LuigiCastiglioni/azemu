package tfstatic

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"

	"github.com/zclconf/go-cty/cty"
)

type modOut struct {
	addr      string
	resources []map[string]any
	children  []*modOut
}

func (m *modOut) json(root bool) map[string]any {
	out := map[string]any{"resources": nonNil(m.resources)}
	if !root {
		out["address"] = m.addr
	}
	if len(m.children) > 0 {
		kids := make([]any, len(m.children))
		for i, k := range m.children {
			kids[i] = k.json(false)
		}
		out["child_modules"] = kids
	}
	return out
}

func nonNil(r []map[string]any) []map[string]any {
	if r == nil {
		return []map[string]any{}
	}
	return r
}

// planJSON renders every azurerm_ managed resource instance as `terraform show -json` would.
// Maps are sorted by encoding/json and resources by address, so equal inputs give equal bytes.
func (e *ev) planJSON() ([]byte, error) {
	nodes := map[string]*modOut{}
	var changes []map[string]any
	for _, m := range e.insts {
		n := &modOut{addr: m.addr}
		nodes[m.addr] = n
		if m.parent != nil {
			p := nodes[m.parent.addr]
			p.children = append(p.children, n)
		}
		for _, k := range sortedKeys(m.cfg.res) {
			rc := m.cfg.res[k]
			if rc.data || !strings.HasPrefix(rc.typ, "azurerm_") {
				continue
			}
			for _, ri := range e.resSet(m, rc).insts {
				known, unknown := split(e.object(ri, &need{attrs: boolSet(ri.extra)}))
				r := map[string]any{"address": ri.addr, "mode": "managed", "type": rc.typ, "name": rc.name,
					"provider_name": "registry.terraform.io/hashicorp/azurerm", "values": known}
				c := map[string]any{"address": ri.addr, "mode": "managed", "type": rc.typ, "name": rc.name,
					"provider_name": "registry.terraform.io/hashicorp/azurerm",
					"change":        map[string]any{"actions": []string{"create"}, "before": nil, "after": known, "after_unknown": unknownOrEmpty(unknown)}}
				if m.addr != "" {
					c["module_address"] = m.addr
				}
				switch {
				case ri.item.key == cty.NilVal:
				case ri.item.suffix[1] == '"':
					r["index"], c["index"] = ri.item.key.AsString(), ri.item.key.AsString()
				default:
					i, _ := ri.item.key.AsBigFloat().Int64()
					r["index"], c["index"] = i, i
				}
				n.resources = append(n.resources, r)
				changes = append(changes, c)
			}
		}
		sortBy(n.resources)
	}
	sortBy(changes)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	err := enc.Encode(map[string]any{
		"format_version": "1.2",
		"planned_values": map[string]any{"root_module": nodes[""].json(true)},
		"resource_changes": func() any {
			if changes == nil {
				return []any{}
			}
			return changes
		}(),
	})
	return buf.Bytes(), err
}

func sortBy(rs []map[string]any) {
	sort.Slice(rs, func(i, j int) bool { return rs[i]["address"].(string) < rs[j]["address"].(string) })
}

func boolSet(m map[string]cty.Value) map[string]bool {
	s := make(map[string]bool, len(m))
	for k := range m {
		s[k] = true
	}
	return s
}

func unknownOrEmpty(u any) any {
	if u == nil {
		return map[string]any{}
	}
	return u
}

// split turns v into its JSON-able known part (unknown attributes omitted, unknown list elements
// null) and terraform's after_unknown mask (nil when v is fully known, true when wholly unknown).
func split(v cty.Value) (known, unknown any) {
	switch t := v.Type(); {
	case !v.IsKnown():
		return nil, true
	case v.IsNull():
		return nil, nil
	case t == cty.String:
		return v.AsString(), nil
	case t == cty.Bool:
		return v.True(), nil
	case t == cty.Number:
		return json.Number(v.AsBigFloat().Text('f', -1)), nil
	case t.IsListType() || t.IsSetType() || t.IsTupleType():
		ks, us, any := []any{}, []any{}, false
		for it := v.ElementIterator(); it.Next(); {
			_, el := it.Element()
			k, u := split(el)
			ks = append(ks, k)
			if u == nil {
				u = false
			}
			us = append(us, u)
			any = any || u != false
		}
		if any {
			return ks, us
		}
		return ks, nil
	case t.IsMapType() || t.IsObjectType():
		ks, us := map[string]any{}, map[string]any{}
		for it := v.ElementIterator(); it.Next(); {
			k, el := it.Element()
			kv, u := split(el)
			if el.IsKnown() {
				ks[k.AsString()] = kv
			}
			if u != nil {
				us[k.AsString()] = u
			}
		}
		if len(us) > 0 {
			return ks, us
		}
		return ks, nil
	}
	return nil, nil
}

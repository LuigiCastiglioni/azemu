package tfstatic

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hashicorp/go-cty-funcs/cidr"
	"github.com/hashicorp/go-cty-funcs/collection"
	"github.com/hashicorp/go-cty-funcs/encoding"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/ext/tryfunc"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
	"github.com/zclconf/go-cty/cty/function/stdlib"
)

// fn builds a function with fixed parameter types and a fixed return type.
func fn(ret cty.Type, impl func(a []cty.Value) (cty.Value, error), params ...cty.Type) function.Function {
	ps := make([]function.Parameter, len(params))
	for i, t := range params {
		ps[i] = function.Parameter{Name: fmt.Sprintf("a%d", i), Type: t}
	}
	return function.New(&function.Spec{
		Params: ps,
		Type:   function.StaticReturnType(ret),
		Impl:   func(a []cty.Value, _ cty.Type) (cty.Value, error) { return impl(a) },
	})
}

func str1(f func(cty.Value) (cty.Value, error)) function.Function {
	return fn(cty.String, func(a []cty.Value) (cty.Value, error) { return f(a[0]) }, cty.String)
}

func hashFn(h func() hash.Hash) function.Function {
	return str1(func(s cty.Value) (cty.Value, error) {
		x := h()
		x.Write([]byte(s.AsString()))
		return cty.StringVal(hex.EncodeToString(x.Sum(nil))), nil
	})
}

// passThrough passes any value through (sensitive/nonsensitive: we emit plaintext, like `terraform show -json`).
var passThrough = function.New(&function.Spec{
	Params: []function.Parameter{{Name: "v", Type: cty.DynamicPseudoType, AllowUnknown: true, AllowNull: true, AllowDynamicType: true}},
	Type:   func(a []cty.Value) (cty.Type, error) { return a[0].Type(), nil },
	Impl:   func(a []cty.Value, _ cty.Type) (cty.Value, error) { return a[0], nil },
})

// parseResourceID is the azurerm provider function of the same name for plain
// /subscriptions/../resourceGroups/..[/providers/<ns>/<type>/<name>[/<type>/<name>...]] ids.
// ponytail: other provider functions stay unsupported (unknown + warning).
var parseResourceID = function.New(&function.Spec{
	Params: []function.Parameter{{Name: "id", Type: cty.String}},
	Type: function.StaticReturnType(cty.Object(map[string]cty.Type{
		"subscription_id": cty.String, "resource_group_name": cty.String, "resource_provider": cty.String,
		"resource_type": cty.String, "resource_name": cty.String, "resource_scope": cty.String, "parent_resources": cty.Map(cty.String),
	})),
	Impl: func(a []cty.Value, _ cty.Type) (cty.Value, error) {
		seg := strings.Split(strings.Trim(a[0].AsString(), "/"), "/")
		if len(seg) < 4 || !strings.EqualFold(seg[0], "subscriptions") || !strings.EqualFold(seg[2], "resourceGroups") || (len(seg) > 4 && (len(seg) < 8 || len(seg)%2 != 0 || !strings.EqualFold(seg[4], "providers"))) {
			return cty.NilVal, fmt.Errorf("%q is not a resource id", a[0].AsString())
		}
		provider, typ, name := "", "resourceGroups", seg[3]
		parents := map[string]cty.Value{}
		if len(seg) > 4 {
			provider, typ, name = seg[5], seg[len(seg)-2], seg[len(seg)-1]
			for i := 6; i < len(seg)-2; i += 2 {
				parents[seg[i]] = cty.StringVal(seg[i+1])
			}
		}
		pr := cty.MapValEmpty(cty.String)
		if len(parents) > 0 {
			pr = cty.MapVal(parents)
		}
		return cty.ObjectVal(map[string]cty.Value{
			"subscription_id": cty.StringVal(seg[1]), "resource_group_name": cty.StringVal(seg[3]), "resource_provider": cty.StringVal(provider),
			"resource_type": cty.StringVal(typ), "resource_name": cty.StringVal(name), "resource_scope": cty.StringVal(""), "parent_resources": pr,
		}), nil
	},
})

func boolList(f func(vals []cty.Value) bool) function.Function {
	return fn(cty.Bool, func(a []cty.Value) (cty.Value, error) {
		var vs []cty.Value
		for it := a[0].ElementIterator(); it.Next(); {
			_, v := it.Element()
			vs = append(vs, v)
		}
		return cty.BoolVal(f(vs)), nil
	}, cty.DynamicPseudoType)
}

// funcs returns Terraform's function set (the subset that makes sense without providers/state).
// File functions resolve relative paths against the root dir, like terraform does against its cwd.
// ponytail: no bcrypt/rsadecrypt/yaml*/other provider::* functions; an expression using one becomes unknown + warning.
func funcs(baseDir string) map[string]function.Function {
	abs := func(p string) string {
		if filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(baseDir, p)
	}
	read := func(p string) ([]byte, error) { return os.ReadFile(abs(p)) }
	uuidN := 0

	f := map[string]function.Function{
		"abs": stdlib.AbsoluteFunc, "ceil": stdlib.CeilFunc, "floor": stdlib.FloorFunc, "log": stdlib.LogFunc,
		"max": stdlib.MaxFunc, "min": stdlib.MinFunc, "parseint": stdlib.ParseIntFunc, "pow": stdlib.PowFunc, "signum": stdlib.SignumFunc,
		"chomp": stdlib.ChompFunc, "indent": stdlib.IndentFunc, "join": stdlib.JoinFunc, "lower": stdlib.LowerFunc, "upper": stdlib.UpperFunc,
		"regex": stdlib.RegexFunc, "regexall": stdlib.RegexAllFunc, "split": stdlib.SplitFunc, "substr": stdlib.SubstrFunc,
		"title": stdlib.TitleFunc, "trim": stdlib.TrimFunc, "trimprefix": stdlib.TrimPrefixFunc, "trimsuffix": stdlib.TrimSuffixFunc,
		"trimspace": stdlib.TrimSpaceFunc, "strrev": stdlib.ReverseFunc, "reverse": stdlib.ReverseListFunc, "sort": stdlib.SortFunc,
		"setintersection": stdlib.SetIntersectionFunc, "setunion": stdlib.SetUnionFunc, "setsubtract": stdlib.SetSubtractFunc,
		"setproduct": stdlib.SetProductFunc, "range": stdlib.RangeFunc, "slice": stdlib.SliceFunc, "element": stdlib.ElementFunc,
		"flatten": stdlib.FlattenFunc, "keys": stdlib.KeysFunc, "values": stdlib.ValuesFunc, "length": stdlib.LengthFunc,
		"lookup": stdlib.LookupFunc, "merge": stdlib.MergeFunc, "zipmap": stdlib.ZipmapFunc, "compact": stdlib.CompactFunc,
		"concat": stdlib.ConcatFunc, "contains": stdlib.ContainsFunc, "distinct": stdlib.DistinctFunc, "chunklist": stdlib.ChunklistFunc,
		"coalesce": collection.CoalesceFunc, "coalescelist": stdlib.CoalesceListFunc, "index": stdlib.IndexFunc,
		"formatdate": stdlib.FormatDateFunc, "timeadd": stdlib.TimeAddFunc, "format": stdlib.FormatFunc, "formatlist": stdlib.FormatListFunc,
		"jsonencode": stdlib.JSONEncodeFunc, "jsondecode": stdlib.JSONDecodeFunc, "csvdecode": stdlib.CSVDecodeFunc,
		"tostring": stdlib.MakeToFunc(cty.String), "tonumber": stdlib.MakeToFunc(cty.Number), "tobool": stdlib.MakeToFunc(cty.Bool),
		"tolist": stdlib.MakeToFunc(cty.List(cty.DynamicPseudoType)), "toset": stdlib.MakeToFunc(cty.Set(cty.DynamicPseudoType)),
		"tomap": stdlib.MakeToFunc(cty.Map(cty.DynamicPseudoType)),
		"try":   tryfunc.TryFunc, "can": tryfunc.CanFunc,
		"sensitive": passThrough, "nonsensitive": passThrough,
		"md5": hashFn(md5.New), "sha1": hashFn(sha1.New), "sha256": hashFn(sha256.New), "sha512": hashFn(sha512.New),
		"base64encode": str1(encoding.Base64Encode), "base64decode": str1(encoding.Base64Decode), "urlencode": str1(encoding.URLEncode),
		"cidrhost":    fn(cty.String, func(a []cty.Value) (cty.Value, error) { return cidr.Host(a[0], a[1]) }, cty.String, cty.Number),
		"cidrnetmask": str1(cidr.Netmask),
		"cidrsubnet":  fn(cty.String, func(a []cty.Value) (cty.Value, error) { return cidr.Subnet(a[0], a[1], a[2]) }, cty.String, cty.Number, cty.Number),
		"cidrsubnets": function.New(&function.Spec{
			Params:   []function.Parameter{{Name: "prefix", Type: cty.String}},
			VarParam: &function.Parameter{Name: "newbits", Type: cty.Number},
			Type:     function.StaticReturnType(cty.List(cty.String)),
			Impl:     func(a []cty.Value, _ cty.Type) (cty.Value, error) { return cidr.Subnets(a[0], a[1:]...) },
		}),
		"alltrue": boolList(func(vs []cty.Value) bool {
			for _, v := range vs {
				if v.IsNull() || !v.True() {
					return false
				}
			}
			return true
		}),
		"anytrue": boolList(func(vs []cty.Value) bool {
			for _, v := range vs {
				if !v.IsNull() && v.True() {
					return true
				}
			}
			return false
		}),
		"one": function.New(&function.Spec{
			Params: []function.Parameter{{Name: "list", Type: cty.DynamicPseudoType}},
			Type: func(a []cty.Value) (cty.Type, error) {
				t := a[0].Type()
				switch {
				case t.IsListType() || t.IsSetType():
					return t.ElementType(), nil
				case t.IsTupleType() && len(t.TupleElementTypes()) == 1:
					return t.TupleElementTypes()[0], nil
				case t.IsTupleType() && len(t.TupleElementTypes()) == 0:
					return cty.DynamicPseudoType, nil
				}
				return cty.NilType, fmt.Errorf("must be a list, set or tuple of at most one element")
			},
			Impl: func(a []cty.Value, ret cty.Type) (cty.Value, error) {
				if a[0].LengthInt() == 0 {
					return cty.NullVal(ret), nil
				}
				if a[0].LengthInt() > 1 {
					return cty.NilVal, fmt.Errorf("must be a list, set or tuple of at most one element")
				}
				it := a[0].ElementIterator()
				it.Next()
				_, v := it.Element()
				return v, nil
			},
		}),
		"startswith": fn(cty.Bool, func(a []cty.Value) (cty.Value, error) {
			return cty.BoolVal(strings.HasPrefix(a[0].AsString(), a[1].AsString())), nil
		}, cty.String, cty.String),
		"endswith": fn(cty.Bool, func(a []cty.Value) (cty.Value, error) {
			return cty.BoolVal(strings.HasSuffix(a[0].AsString(), a[1].AsString())), nil
		}, cty.String, cty.String),
		"strcontains": fn(cty.Bool, func(a []cty.Value) (cty.Value, error) {
			return cty.BoolVal(strings.Contains(a[0].AsString(), a[1].AsString())), nil
		}, cty.String, cty.String),
		"replace": fn(cty.String, func(a []cty.Value) (cty.Value, error) {
			s, old, rep := a[0].AsString(), a[1].AsString(), a[2].AsString()
			if len(old) > 1 && strings.HasPrefix(old, "/") && strings.HasSuffix(old, "/") {
				re, err := regexp.Compile(old[1 : len(old)-1])
				if err != nil {
					return cty.NilVal, err
				}
				return cty.StringVal(re.ReplaceAllString(s, rep)), nil
			}
			return cty.StringVal(strings.ReplaceAll(s, old, rep)), nil
		}, cty.String, cty.String, cty.String),
		"timestamp":     fn(cty.String, func([]cty.Value) (cty.Value, error) { return cty.StringVal("2000-01-01T00:00:00Z"), nil }),
		"plantimestamp": fn(cty.String, func([]cty.Value) (cty.Value, error) { return cty.StringVal("2000-01-01T00:00:00Z"), nil }),
		// deterministic: same inputs -> same output, like everything else here
		"uuid": fn(cty.String, func([]cty.Value) (cty.Value, error) {
			uuidN++
			h := sha256.Sum256([]byte(fmt.Sprint("uuid", uuidN)))
			x := hex.EncodeToString(h[:16])
			return cty.StringVal(x[:8] + "-" + x[8:12] + "-4" + x[13:16] + "-a" + x[17:20] + "-" + x[20:32]), nil
		}),
		"file": str1(func(p cty.Value) (cty.Value, error) {
			b, err := read(p.AsString())
			return cty.StringVal(string(b)), err
		}),
		"filebase64": str1(func(p cty.Value) (cty.Value, error) {
			b, err := read(p.AsString())
			return cty.StringVal(base64.StdEncoding.EncodeToString(b)), err
		}),
		"fileexists": fn(cty.Bool, func(a []cty.Value) (cty.Value, error) {
			st, err := os.Stat(abs(a[0].AsString()))
			return cty.BoolVal(err == nil && !st.IsDir()), nil
		}, cty.String),
		"abspath":  str1(func(p cty.Value) (cty.Value, error) { return cty.StringVal(abs(p.AsString())), nil }),
		"dirname":  str1(func(p cty.Value) (cty.Value, error) { return cty.StringVal(filepath.Dir(p.AsString())), nil }),
		"basename": str1(func(p cty.Value) (cty.Value, error) { return cty.StringVal(filepath.Base(p.AsString())), nil }),
	}
	f["provider::azurerm::parse_resource_id"] = parseResourceID
	f["templatefile"] = function.New(&function.Spec{
		Params: []function.Parameter{{Name: "path", Type: cty.String}, {Name: "vars", Type: cty.DynamicPseudoType}},
		Type:   function.StaticReturnType(cty.String),
		Impl: func(a []cty.Value, _ cty.Type) (cty.Value, error) {
			p := abs(a[0].AsString())
			b, err := os.ReadFile(p)
			if err != nil {
				return cty.NilVal, err
			}
			if !a[1].Type().IsObjectType() && !a[1].Type().IsMapType() {
				return cty.NilVal, fmt.Errorf("vars must be an object or map")
			}
			expr, diags := hclsyntax.ParseTemplate(b, p, hcl.Pos{Line: 1, Column: 1})
			if diags.HasErrors() {
				return cty.NilVal, fmt.Errorf("%s", diags.Error())
			}
			v, diags := expr.Value(&hcl.EvalContext{Variables: a[1].AsValueMap(), Functions: f})
			if diags.HasErrors() {
				return cty.NilVal, fmt.Errorf("%s", diags.Error())
			}
			return v, nil
		},
	})
	return f
}

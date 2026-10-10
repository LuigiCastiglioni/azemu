package discover

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// scope is the variable/parameter context of the file being walked.
type scope struct {
	params, vars map[string]string
	file         string // relative to scan root, for Source
	dir          string // absolute dir of the file, to resolve template: paths
	depth        int
}

type scanner struct {
	root, env string
	archives  map[string]string // ArchiveFiles@2 archiveFile -> rootFolderOrFile
	found     []Finding
	warns     []string

	tfDir, tfSrc      string
	tfFromPlan        bool
	tfVars, tfBackend []string
}

var (
	macroRe = regexp.MustCompile(`\$\(([A-Za-z0-9_.\-]+)\)`)
	exprRe  = regexp.MustCompile(`\$\{\{\s*(parameters|variables)\.([A-Za-z0-9_\-]+)\s*\}\}`)
	// predefined variables that denote the repo root: drop them so paths come out relative
	rootVarRe = regexp.MustCompile(`\$\((?:Build\.SourcesDirectory|System\.DefaultWorkingDirectory|Build\.Repository\.LocalPath)\)[/\\]?`)
)

// sub resolves ${{ parameters.x }}, ${{ variables.x }} and $(var), to a fixpoint (max 5 rounds).
// Unresolved tokens stay as they are.
func (c *scope) sub(s string) string {
	for i := 0; i < 5; i++ {
		n := exprRe.ReplaceAllStringFunc(s, func(m string) string {
			g := exprRe.FindStringSubmatch(m)
			src := c.params
			if g[1] == "variables" {
				src = c.vars
			}
			if v, ok := src[g[2]]; ok {
				return v
			}
			return m
		})
		n = macroRe.ReplaceAllStringFunc(n, func(m string) string {
			if v, ok := c.vars[m[2:len(m)-1]]; ok {
				return v
			}
			return m
		})
		if n == s {
			break
		}
		s = n
	}
	return rootVarRe.ReplaceAllString(s, "")
}

func loadYAML(p string) (*yaml.Node, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		return doc.Content[0], nil
	}
	return nil, nil
}

func mapGet(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

func scalarOf(n *yaml.Node, key string) string {
	if v := mapGet(n, key); v != nil && v.Kind == yaml.ScalarNode {
		return v.Value
	}
	return ""
}

// paramDefaults reads `parameters:` in list form (- name/default) or map form.
func paramDefaults(doc *yaml.Node) map[string]string {
	out := map[string]string{}
	p := mapGet(doc, "parameters")
	if p == nil {
		return out
	}
	switch p.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(p.Content); i += 2 {
			if p.Content[i+1].Kind == yaml.ScalarNode {
				out[p.Content[i].Value] = p.Content[i+1].Value
			}
		}
	case yaml.SequenceNode:
		for _, it := range p.Content {
			if d := mapGet(it, "default"); d != nil && d.Kind == yaml.ScalarNode {
				out[scalarOf(it, "name")] = d.Value
			}
		}
	}
	return out
}

// collectVars gathers every `variables:` block in doc (any depth; first definition wins).
// Variable groups and templates are unresolved by design. ponytail: no job/stage scoping.
func collectVars(n *yaml.Node, out map[string]string) {
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == "variables" {
				addVars(n.Content[i+1], out)
			}
		}
	}
	for _, ch := range n.Content {
		collectVars(ch, out)
	}
}

func addVars(v *yaml.Node, out map[string]string) {
	set := func(k, val string) {
		if _, ok := out[k]; !ok && k != "" {
			out[k] = val
		}
	}
	switch v.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(v.Content); i += 2 {
			val := v.Content[i+1]
			if val.Kind == yaml.ScalarNode {
				set(v.Content[i].Value, val.Value)
			} else if x := scalarOf(val, "value"); x != "" {
				set(v.Content[i].Value, x)
			}
		}
	case yaml.SequenceNode:
		for _, it := range v.Content {
			if mapGet(it, "value") != nil {
				set(scalarOf(it, "name"), scalarOf(it, "value"))
			}
		}
	}
}

// addScopeVars merges the file's variables into c.vars, resolving ${{ }} with c's own params.
func (c *scope) addScopeVars(doc *yaml.Node) {
	nv := map[string]string{}
	collectVars(doc, nv)
	for k, v := range nv {
		if _, ok := c.vars[k]; !ok {
			c.vars[k] = exprRe.ReplaceAllStringFunc(v, func(m string) string {
				if g := exprRe.FindStringSubmatch(m); g[1] == "parameters" {
					if pv, ok := c.params[g[2]]; ok {
						return pv
					}
				}
				return m
			})
		}
	}
}

func (s *scanner) warn(f string, a ...any) { s.warns = append(s.warns, fmt.Sprintf(f, a...)) }

func (s *scanner) scanFile(p string) {
	doc, err := loadYAML(p)
	if err != nil {
		s.warn("%v", err)
		return
	}
	if doc == nil {
		return
	}
	c := &scope{params: paramDefaults(doc), vars: map[string]string{}, file: rel(s.root, p), dir: filepath.Dir(p)}
	c.addScopeVars(doc)
	s.walk(doc, c)
}

var scriptKeys = map[string]bool{"script": true, "bash": true, "pwsh": true, "powershell": true}

func (s *scanner) walk(n *yaml.Node, c *scope) {
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, ch := range n.Content {
			s.walk(ch, c)
		}
	case yaml.MappingNode:
		if t := mapGet(n, "template"); t != nil && t.Kind == yaml.ScalarNode {
			s.expand(n, t, c)
			return
		}
		if t := mapGet(n, "task"); t != nil && t.Kind == yaml.ScalarNode {
			s.task(n, c.sub(t.Value), c)
			return
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			if k := n.Content[i].Value; scriptKeys[k] && n.Content[i+1].Kind == yaml.ScalarNode {
				s.scriptStep(n, c.sub(n.Content[i+1].Value), k, scalarOf(n, "workingDirectory"), c)
				return
			}
		}
		s.pairs(n, c)
		for i := 1; i < len(n.Content); i += 2 {
			s.walk(n.Content[i], c)
		}
	}
}

// expand inlines `- template: x.yml` with its parameter defaults overridden by the passed ones.
// ponytail: depth capped at 3 includes; `template: x@repo` (external repo) is skipped.
func (s *scanner) expand(n, t *yaml.Node, c *scope) {
	if c.depth >= 3 || strings.Contains(t.Value, "@") {
		return
	}
	ref := c.sub(t.Value)
	p := filepath.Join(c.dir, filepath.FromSlash(ref))
	if strings.HasPrefix(ref, "/") {
		p = filepath.Join(s.root, filepath.FromSlash(ref))
	}
	doc, err := loadYAML(p)
	if err != nil || doc == nil {
		s.warn("%s:%d template %s: not readable", c.file, t.Line, ref)
		return
	}
	nc := &scope{params: paramDefaults(doc), vars: map[string]string{}, file: rel(s.root, p), dir: filepath.Dir(p), depth: c.depth + 1}
	for k, v := range c.vars {
		nc.vars[k] = v
	}
	if pm := mapGet(n, "parameters"); pm != nil && pm.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(pm.Content); i += 2 {
			if pm.Content[i+1].Kind == yaml.ScalarNode {
				nc.params[pm.Content[i].Value] = c.sub(pm.Content[i+1].Value)
			}
		}
	}
	nc.addScopeVars(doc)
	s.walk(doc, nc)
}

// pairs finds mappings that carry a <x>AppName and <x>AppPath key (matrix entries).
func (s *scanner) pairs(n *yaml.Node, c *scope) {
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		low := strings.ToLower(k.Value)
		if !strings.HasSuffix(low, "appname") || v.Kind != yaml.ScalarNode {
			continue
		}
		pk := strings.TrimSuffix(low, "appname") + "apppath"
		for j := 0; j+1 < len(n.Content); j += 2 {
			if strings.ToLower(n.Content[j].Value) == pk && n.Content[j+1].Kind == yaml.ScalarNode {
				s.emit(Finding{AppName: c.sub(v.Value), pkg: c.sub(n.Content[j+1].Value)},
					fmt.Sprintf("%s:%d %s/%s", c.file, k.Line, k.Value, n.Content[j].Value), "medium")
			}
		}
	}
}

// emit records a finding; confidence drops to low when any field is still unresolved.
func (s *scanner) emit(f Finding, src, base string) {
	f.Source, f.Confidence = src, base
	for _, v := range []string{f.AppName, f.Path, f.Image, f.Dockerfile} { // pkg is judged once resolved, in finish()
		if hasUnresolved(v) {
			f.Confidence = "low"
		}
	}
	s.found = append(s.found, f)
}

func (s *scanner) inputs(n *yaml.Node, c *scope) map[string]string {
	in := map[string]string{}
	for _, holder := range []*yaml.Node{n, mapGet(n, "inputs")} {
		if holder == nil || holder.Kind != yaml.MappingNode {
			continue
		}
		for i := 0; i+1 < len(holder.Content); i += 2 {
			if v := holder.Content[i+1]; v.Kind == yaml.ScalarNode {
				k := strings.ToLower(holder.Content[i].Value)
				if _, ok := in[k]; !ok {
					in[k] = c.sub(v.Value)
				}
			}
		}
	}
	return in
}

// task handles `- task: Name@ver`.
func (s *scanner) task(n *yaml.Node, task string, c *scope) {
	name := strings.ToLower(strings.SplitN(task, "@", 2)[0])
	in := s.inputs(n, c)
	src := fmt.Sprintf("%s:%d %s", c.file, n.Line, task)
	wd := in["workingdirectory"]
	switch {
	case name == "azurefunctionapp" || name == "azurewebapp":
		f := Finding{AppName: in["appname"], pkg: in["package"], Kind: "zip"}
		if img := imageRepo(in["imagename"]); img != "" {
			f.Image, f.Kind, f.pkg = img, "docker", ""
		}
		s.emit(f, src, "high")
	case name == "azurewebappcontainer":
		img := in["imagename"]
		if img == "" {
			if f := strings.Fields(in["containers"]); len(f) > 0 {
				img = f[0]
			}
		}
		s.emit(Finding{AppName: in["appname"], Image: imageRepo(img), Kind: "docker"}, src, "high")
	case name == "azurermwebappdeployment":
		f := Finding{AppName: in["webappname"], pkg: in["packageforlinux"], Kind: "zip"}
		if r := in["dockerrepository"]; r != "" {
			f.Image, f.Kind, f.pkg = imageRepo(r), "docker", ""
		}
		s.emit(f, src, "high")
	case name == "docker":
		s.dockerTask(in, src)
	case name == "archivefiles":
		if root, file := in["rootfolderorfile"], in["archivefile"]; root != "" && file != "" {
			s.archives[normPath(file)] = normPath(root)
		}
	case strings.HasPrefix(name, "terraform"):
		s.terraformTask(in, src)
	default:
		text := in["inlinescript"]
		if text == "" {
			text = in["script"]
		}
		if fp := first(in["filepath"], in["scriptpath"]); fp != "" {
			text += "\n& '" + fp + "' " + in["arguments"]
		}
		if text != "" {
			s.scriptText(text, wd, src, "medium")
		}
	}
}

func first(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

func (s *scanner) scriptStep(n *yaml.Node, text, key, wd string, c *scope) {
	s.scriptText(text, c.sub(wd), fmt.Sprintf("%s:%d %s", c.file, n.Line, key), "medium")
}

func (s *scanner) dockerTask(in map[string]string, src string) {
	cmd := strings.ToLower(first(in["command"], "buildandpush"))
	if cmd != "build" && cmd != "buildandpush" {
		return
	}
	df := first(in["dockerfile"], "**/Dockerfile")
	ctx := in["buildcontext"]
	if ctx == "" || ctx == "**" {
		ctx = "."
		if !strings.Contains(df, "*") {
			ctx = path.Dir(normPath(df))
		}
	}
	ctx = normPath(ctx)
	f := Finding{Path: ctx, Image: imageRepo(in["repository"]), Kind: "docker"}
	if !strings.Contains(df, "*") {
		f.Dockerfile = strings.TrimPrefix(normPath(df), ctx+"/")
		if ctx == "." {
			f.Dockerfile = normPath(df)
		}
	}
	s.emit(f, src, "high")
}

func (s *scanner) terraformTask(in map[string]string, src string) {
	cmd := strings.ToLower(in["command"])
	if wd := in["workingdirectory"]; wd != "" && (cmd == "plan" || cmd == "apply" || cmd == "init") {
		s.setTFDir(normPath(wd), src, cmd == "plan")
	}
	s.terraformFlags(cmd, tokenize(in["commandoptions"]), src)
}

// setTFDir records the terraform directory; a plan step beats the first init/apply seen.
func (s *scanner) setTFDir(dir, src string, plan bool) {
	if s.tfDir == "" || (plan && !s.tfFromPlan) {
		s.tfDir, s.tfSrc, s.tfFromPlan = dir, src, plan
	}
}

// terraformFlags collects -var-file from plan and -backend-config from init.
func (s *scanner) terraformFlags(cmd string, toks []string, src string) {
	flags, _ := parseFlags(toks)
	if cmd == "plan" {
		s.tfVars = append(s.tfVars, flags["-var-file"]...)
	}
	if cmd == "init" {
		for _, b := range flags["-backend-config"] {
			if strings.ContainsAny(b, "/.") && !strings.Contains(b, "=") {
				s.tfBackend = append(s.tfBackend, b)
			}
		}
	}
}

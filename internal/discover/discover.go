// Package discover proposes azemu.yaml from Azure DevOps pipelines and the infra model.
//
// Flow: FindPipelines -> Scan (parse, resolve, extract Findings + terraform args)
// -> Match (join to infra.Model) -> Draft -> Render / MergeInto.
package discover

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Finding is one "this folder/image deploys to this app" fact found in a pipeline.
type Finding struct {
	AppName    string // resolved Azure resource name; may still contain $(var) when unresolved
	Path       string // build context / package folder, relative to the scanned root
	Dockerfile string // relative to Path
	Image      string // image repository without registry and tag
	Kind       string // "zip" | "docker" | "" (unknown)
	Port       int    // WEBSITES_PORT passed by a script, 0 if unknown
	Source     string // "<file>:<line> <task>"
	Confidence string // "high" | "medium" | "low"

	pkg string // zip package reference, resolved to Path through ArchiveFiles@2 in finish()
}

// TF is the terraform invocation discovered in infra pipelines.
type TF struct {
	Dir           string   // relative to the scanned root
	VarFiles      []string // relative to Dir, env replaced with {env}
	BackendConfig []string // relative to Dir, env replaced with {env}
	Source        string
	Confidence    string
}

type Result struct {
	Findings  []Finding
	TF        *TF // nil when no terraform step was found
	Pipelines []string
	Warnings  []string
}

var skipDirs = map[string]bool{"node_modules": true, ".git": true, ".venv": true, ".azemu": true}

// FindPipelines lists azure-pipelines*.yml|yaml anywhere under root, plus *.yml|yaml
// below directories named pipelines or .azure-pipelines.
func FindPipelines(root string) []string {
	var out []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] && p != root {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(p))
		if ext != ".yml" && ext != ".yaml" {
			return nil
		}
		if strings.HasPrefix(strings.ToLower(d.Name()), "azure-pipelines") || inPipelineDir(root, p) {
			out = append(out, p)
		}
		return nil
	})
	return out
}

func inPipelineDir(root, p string) bool {
	rel, err := filepath.Rel(root, filepath.Dir(p))
	if err != nil {
		return false
	}
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		if seg == "pipelines" || seg == ".azure-pipelines" {
			return true
		}
	}
	return false
}

var envNames = map[string]bool{"dev": true, "test": true, "prod": true, "uat": true, "stg": true, "staging": true, "qa": true, "preprod": true, "int": true}

// filterEnv drops pipelines whose file name names a different environment than env.
// ponytail: file-name heuristic only; an `environment:` match is not consulted.
func filterEnv(files []string, env string) []string {
	var keep []string
	for _, f := range files {
		other, mine := false, false
		for _, t := range splitTokens(strings.TrimSuffix(filepath.Base(f), filepath.Ext(f))) {
			if t == env {
				mine = true
			} else if envNames[t] {
				other = true
			}
		}
		if mine || !other {
			keep = append(keep, f)
		}
	}
	if len(keep) == 0 {
		return files
	}
	return keep
}

var tokRe = regexp.MustCompile(`[^a-z0-9]+`)

func splitTokens(s string) []string {
	var out []string
	for _, t := range tokRe.Split(strings.ToLower(s), -1) {
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

// entryPoints drops files that another pipeline includes via `template:`; those are
// scanned in the including file's context (with the passed parameters).
func entryPoints(files []string) []string {
	ref := map[string]bool{}
	for _, f := range files {
		doc, err := loadYAML(f)
		if err != nil || doc == nil {
			continue
		}
		var visit func(n *yaml.Node)
		visit = func(n *yaml.Node) {
			if n.Kind == yaml.MappingNode {
				for i := 0; i+1 < len(n.Content); i += 2 {
					if n.Content[i].Value == "template" && n.Content[i+1].Kind == yaml.ScalarNode {
						v := n.Content[i+1].Value
						if !strings.ContainsAny(v, "@$") {
							ref[filepath.Clean(filepath.Join(filepath.Dir(f), filepath.FromSlash(v)))] = true
						}
					}
				}
			}
			for _, c := range n.Content {
				visit(c)
			}
		}
		visit(doc)
	}
	var out []string
	for _, f := range files {
		if !ref[filepath.Clean(f)] {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return files
	}
	return out
}

// Scan finds and analyzes the pipelines under root for env.
func Scan(root, env string) (*Result, error) {
	files := filterEnv(entryPoints(FindPipelines(root)), env)
	s := &scanner{root: root, env: env, archives: map[string]string{}}
	for _, f := range files {
		s.scanFile(f)
	}
	r := s.finish()
	for _, f := range files {
		r.Pipelines = append(r.Pipelines, rel(root, f))
	}
	r.Warnings = s.warns
	return r, nil
}

func rel(root, p string) string {
	if r, err := filepath.Rel(root, p); err == nil {
		return filepath.ToSlash(r)
	}
	return filepath.ToSlash(p)
}

// ResolveInfra maps the --infra argument to (repo root to scan, terraform dir or "").
// A terraform directory (contains *.tf) is scanned from its git root, else its parent.
func ResolveInfra(p string) (root, tfDir string) {
	p, _ = filepath.Abs(p)
	if m, _ := filepath.Glob(filepath.Join(p, "*.tf")); len(m) == 0 {
		return p, ""
	}
	for d := p; d != filepath.Dir(d); d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d, p
		}
	}
	return filepath.Dir(p), p
}

// --- finish: resolve packages, validate, merge ---

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func hasUnresolved(s string) bool { return strings.Contains(s, "$(") || strings.Contains(s, "${{") }

var confRank = map[string]int{"low": 0, "medium": 1, "high": 2}

func (s *scanner) finish() *Result {
	var kept []Finding
	for _, f := range s.found {
		if f.AppName != "" && !hasUnresolved(f.AppName) && !nameRe.MatchString(f.AppName) {
			f.AppName, f.Confidence = "", "low" // display name or script variable, not a resource name
		} else if strings.HasPrefix(f.AppName, "$") && !strings.HasPrefix(f.AppName, "$(") {
			f.AppName = ""
		}
		if f.pkg != "" && f.Path == "" {
			f.Path = s.resolvePkg(f.pkg)
			if f.Path == "" {
				f.Confidence = "low"
			}
		}
		if f.Path != "" && (strings.Contains(f.Path, "$") || strings.ContainsAny(f.Path, "*?")) {
			f.Path, f.Confidence = "", "low"
		}
		if f.Path == "" && f.Image == "" {
			continue // nothing to build from
		}
		kept = append(kept, f)
	}
	// merge findings that share a path or an image repo
	var out []Finding
	for _, f := range kept {
		merged := false
		for i := range out {
			o := &out[i]
			if (f.Path != "" && f.Path == o.Path) || (f.Image != "" && f.Image == o.Image) {
				mergeInto(o, f)
				merged = true
				break
			}
		}
		if !merged {
			out = append(out, f)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return &Result{Findings: out, TF: s.finishTF()}
}

func mergeInto(o *Finding, f Finding) {
	if (o.AppName == "" || hasUnresolved(o.AppName)) && f.AppName != "" && (!hasUnresolved(f.AppName) || o.AppName == "") {
		o.AppName = f.AppName
	}
	if o.Path == "" {
		o.Path = f.Path
	}
	if o.Dockerfile == "" {
		o.Dockerfile = f.Dockerfile
	}
	if o.Image == "" {
		o.Image = f.Image
	}
	if o.Port == 0 {
		o.Port = f.Port
	}
	if f.Kind == "docker" || o.Kind == "" {
		o.Kind = f.Kind
	}
	if confRank[f.Confidence] > confRank[o.Confidence] || (o.Path == "" && f.Path != "") {
		o.Source, o.Confidence = f.Source, f.Confidence
	}
}

// resolvePkg maps a deploy package (folder or zip) back to its source folder through
// ArchiveFiles@2 steps. ponytail: basename glob match; several candidate archives -> unresolved.
func (s *scanner) resolvePkg(pkg string) string {
	pkg = normPath(pkg)
	if src, ok := s.archives[pkg]; ok {
		return src
	}
	g := globOf(path.Base(pkg))
	var hit []string
	for a, src := range s.archives {
		if ok, _ := path.Match(g, path.Base(a)); ok {
			hit = append(hit, src)
		}
	}
	if len(hit) == 1 {
		return hit[0]
	}
	if len(hit) == 0 && !strings.HasSuffix(strings.ToLower(pkg), ".zip") && !strings.ContainsAny(pkg, "*?$") {
		return pkg // a folder
	}
	return ""
}

var macroGlobRe = regexp.MustCompile(`\$\([^)]*\)`)

func globOf(s string) string {
	return strings.ReplaceAll(macroGlobRe.ReplaceAllString(s, "*"), "**", "*")
}

// normPath makes p a clean forward-slash relative path ("" -> ".").
func normPath(p string) string {
	p = strings.Trim(strings.TrimSpace(p), `'"`)
	if hasUnresolved(p) {
		return strings.ReplaceAll(p, `\`, "/")
	}
	p = strings.ReplaceAll(p, `\`, "/")
	if p == "" {
		return "."
	}
	return path.Clean(p)
}

// --- terraform ---

func (s *scanner) finishTF() *TF {
	if s.tfDir == "" && len(s.tfVars) == 0 && len(s.tfBackend) == 0 {
		return nil
	}
	dir := s.tfDir
	if dir == "" {
		dir = "."
	}
	t := &TF{Dir: dir, Source: s.tfSrc, Confidence: "high"}
	norm := func(p string) string {
		p = normPath(p)
		if dir != "." {
			p = strings.TrimPrefix(p, dir+"/")
		}
		if hasUnresolved(p) {
			t.Confidence = "low"
		}
		return envToken(p, s.env)
	}
	for _, v := range s.tfVars {
		t.VarFiles = appendUniq(t.VarFiles, norm(v))
	}
	for _, v := range s.tfBackend {
		t.BackendConfig = appendUniq(t.BackendConfig, norm(v))
	}
	if len(t.BackendConfig) == 0 {
		fallback := "configs/" + s.env + "/backend.tfvars"
		if _, err := os.Stat(filepath.Join(s.root, filepath.FromSlash(dir), filepath.FromSlash(fallback))); err == nil {
			t.BackendConfig = []string{envToken(fallback, s.env)}
			if t.Confidence == "high" {
				t.Confidence = "medium"
			}
		}
	}
	return t
}

// envToken replaces path segments (or their stem) equal to env with {env}.
func envToken(p, env string) string {
	segs := strings.Split(p, "/")
	for i, sg := range segs {
		stem, ext, _ := strings.Cut(sg, ".")
		if sg == env {
			segs[i] = "{env}"
		} else if stem == env && ext != "" {
			segs[i] = "{env}." + ext
		}
	}
	return strings.Join(segs, "/")
}

func appendUniq(l []string, v string) []string {
	for _, x := range l {
		if x == v {
			return l
		}
	}
	return append(l, v)
}

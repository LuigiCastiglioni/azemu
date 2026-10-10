package discover

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/luigi23997/azemu/internal/config"
	"gopkg.in/yaml.v3"
)

// Row is one line of the init summary table.
type Row struct{ Key, Resource, Kind, Source, Confidence string }

type Proposal struct {
	Config   *config.Config
	Comments map[string]string // app key -> "from <source> (<confidence>)"
	Rows     []Row
	Todos    []string // model apps without a pipeline source
}

var exposeRe = regexp.MustCompile(`(?im)^\s*EXPOSE\s+(\d+)`)

// Draft builds the azemu.yaml content. root is the code repo root (to read Dockerfiles).
func Draft(root, env string, inf config.Infra, mr MatchResult) *Proposal {
	d := &Proposal{
		Config:   &config.Config{Env: env, Infra: inf, Secrets: ".env", Apps: map[string]*config.App{}},
		Comments: map[string]string{},
	}
	add := func(f Finding, resource, kind, runtime, comment string, modelPort int) {
		if f.Path == "" {
			return // an app needs a folder; reported as unmatched elsewhere
		}
		key := path.Base(f.Path)
		if f.Path == "." {
			key = filepath.Base(root)
		}
		for n := 2; d.Config.Apps[key] != nil; n++ {
			key = fmt.Sprintf("%s-%d", path.Base(f.Path), n)
		}
		a := &config.App{Path: f.Path, Resource: map[string]string{}, Kind: kind, Runtime: runtime}
		if resource != "" {
			a.Resource[env] = resource
		}
		if kind == "docker" {
			a.Dockerfile = f.Dockerfile
			if a.Dockerfile == "" {
				a.Dockerfile = "Dockerfile"
			}
			a.Port = modelPort
			if a.Port == 0 {
				a.Port = f.Port
			}
			if a.Port == 0 {
				a.Port = exposedPort(filepath.Join(root, filepath.FromSlash(f.Path), filepath.FromSlash(a.Dockerfile)))
			}
		}
		d.Config.Apps[key] = a
		d.Comments[key] = comment
		d.Rows = append(d.Rows, Row{key, resource, kind, f.Source, f.Confidence})
	}
	for _, m := range mr.Matched {
		kind, runtime := m.App.Kind, ""
		if kind == "" {
			kind = first(m.Kind, "zip")
		}
		if kind == "zip" && m.App.Runtime != "" {
			runtime = m.App.Runtime
			if m.App.RuntimeVersion != "" {
				runtime += ":" + m.App.RuntimeVersion
			}
		}
		port, _ := strconv.Atoi(m.App.AppSettings["WEBSITES_PORT"])
		add(m.Finding, m.App.Name, kind, runtime, fmt.Sprintf("# from %s (%s)", m.Source, m.Confidence), port)
	}
	for _, f := range mr.Unmatched {
		res := ""
		if f.AppName != "" && !hasUnresolved(f.AppName) {
			res = f.AppName
		}
		kind := first(f.Kind, "zip")
		add(f, res, kind, "", fmt.Sprintf("# from %s (%s)\n# UNMATCHED: not matched to a terraform app; check resource.%s", f.Source, f.Confidence, d.Config.Env), 0)
	}
	for _, a := range mr.Missing {
		d.Todos = append(d.Todos, fmt.Sprintf("TODO: %s (%s) has no pipeline source; add it under apps:", a.Name, a.Kind))
	}
	sort.Slice(d.Rows, func(i, j int) bool { return d.Rows[i].Key < d.Rows[j].Key })
	return d
}

func exposedPort(dockerfile string) int {
	b, err := os.ReadFile(dockerfile)
	if err != nil {
		return 0
	}
	if m := exposeRe.FindSubmatch(b); m != nil {
		n, _ := strconv.Atoi(string(m[1]))
		return n
	}
	return 0
}

// Render writes the draft as YAML with a source comment above each app.
func (d *Proposal) Render() ([]byte, error) {
	var doc yaml.Node
	if err := doc.Encode(d.Config); err != nil {
		return nil, err
	}
	apps := mapGet(&doc, "apps")
	if apps != nil {
		for i := 0; i+1 < len(apps.Content); i += 2 {
			apps.Content[i].HeadComment = d.Comments[apps.Content[i].Value]
		}
		if len(d.Todos) > 0 {
			apps.FootComment = "# " + strings.Join(d.Todos, "\n# ")
		}
	}
	return marshal(&doc)
}

// MergeInto adds the draft to an existing azemu.yaml (--update): new apps are appended with
// their source comment; for known apps only the missing env resource name is added. Every
// other manual field and comment is kept.
func (d *Proposal) MergeInto(existing []byte, env string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(existing, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("existing azemu.yaml is not a mapping")
	}
	root := doc.Content[0]
	apps := mapGet(root, "apps")
	if apps == nil || apps.Kind != yaml.MappingNode {
		apps = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "apps"}, apps)
	}
	keys := make([]string, 0, len(d.Config.Apps))
	for k := range d.Config.Apps {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		a := d.Config.Apps[k]
		old := mapGet(apps, k)
		if old == nil {
			var v yaml.Node
			if err := v.Encode(a); err != nil {
				return nil, err
			}
			apps.Content = append(apps.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k, HeadComment: d.Comments[k]}, &v)
			continue
		}
		name := a.Resource[env]
		if name == "" || old.Kind != yaml.MappingNode {
			continue
		}
		res := mapGet(old, "resource")
		if res == nil {
			var v yaml.Node
			_ = v.Encode(map[string]string{})
			old.Content = append(old.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "resource"}, &v)
			res = &v
		}
		if res.Kind == yaml.MappingNode && mapGet(res, env) == nil {
			res.Content = append(res.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: env}, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name})
			res.Style = yaml.FlowStyle
		}
	}
	return marshal(&doc)
}

func marshal(n *yaml.Node) ([]byte, error) {
	var b bytes.Buffer
	e := yaml.NewEncoder(&b)
	e.SetIndent(2)
	if err := e.Encode(n); err != nil {
		return nil, err
	}
	return b.Bytes(), e.Close()
}

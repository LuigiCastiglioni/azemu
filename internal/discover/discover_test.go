package discover

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/luigi23997/azemu/internal/config"
	"github.com/luigi23997/azemu/internal/infra"
	"gopkg.in/yaml.v3"
)

// tuple is the comparable core of a Finding: name|path|dockerfile|image|kind.
func tuple(f Finding) string {
	return strings.Join([]string{f.AppName, f.Path, f.Dockerfile, f.Image, f.Kind}, "|")
}

func tuples(fs []Finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, tuple(f))
	}
	sort.Strings(out)
	return out
}

func scan(t *testing.T, dir string) *Result {
	t.Helper()
	r, err := Scan(dir, "dev")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range r.Warnings {
		t.Errorf("warning: %s", w)
	}
	return r
}

func TestTaskExtractors(t *testing.T) {
	tests := []struct {
		dir  string
		want []string // name|path|dockerfile|image|kind
	}{
		{"azfunc", []string{"$(fromGroup)|apps/other|||zip", "fa-dev|apps/fa|||zip"}},
		{"azweb", []string{"web-app-dev|apps/web|||zip"}},
		{"container", []string{"web-ctr-dev|||team/ctr-image|docker"}},
		{"rmdeploy", []string{"web-rm-dev|||rm-image|docker", "web-rm-zip|apps/rmzip|||zip"}},
		{"docker2", []string{"|apps/d2|Dockerfile|team/docker2-app|docker"}},
		{"scripts", []string{
			"|apps/sh-api|docker/Api.Dockerfile|scripted-api|docker",
			"|apps/ps-api|Dockerfile|ps-api|docker",
			"fn-zip-dev|apps/fn-zip|||zip",
			"web-zip-dev|apps/web-zip|||zip",
			"fn-core-dev|apps/fn-core|||zip",
		}},
		{"params", []string{
			"api-dev|apps/api|||zip",
			"h1-dev|apps/h1|||zip",
			"$(fromGroup)|apps/h2|||zip",
			"img-dev|||img-repo|docker",
		}},
		{"template", []string{"tpl-app-dev|apps/tplfolder|||zip", "second-dev|apps/dev-second|||zip"}},
	}
	for _, tc := range tests {
		t.Run(tc.dir, func(t *testing.T) {
			r := scan(t, filepath.Join("testdata", "tasks", tc.dir))
			want := append([]string(nil), tc.want...)
			sort.Strings(want)
			if got := tuples(r.Findings); !reflect.DeepEqual(got, want) {
				t.Errorf("findings\n got %q\nwant %q", got, want)
			}
		})
	}
}

func TestConfidenceAndPort(t *testing.T) {
	r := scan(t, "testdata/tasks/azfunc")
	for _, f := range r.Findings {
		wantConf := map[string]string{"fa-dev": "high", "$(fromGroup)": "low"}[f.AppName]
		if f.Confidence != wantConf {
			t.Errorf("%s: confidence %s, want %s", f.AppName, f.Confidence, wantConf)
		}
	}
	r = scan(t, "testdata/tasks/params")
	for _, f := range r.Findings {
		if f.AppName == "img-dev" && f.Port != 8080 {
			t.Errorf("img-dev port = %d, want 8080", f.Port)
		}
	}
}

func TestTerraformTasks(t *testing.T) {
	r := scan(t, "testdata/tasks/terraform")
	want := &TF{
		Dir:           "terraform",
		VarFiles:      []string{"configs/common.tfvars", "configs/{env}/terraform.tfvars"},
		BackendConfig: []string{"configs/{env}/backend.tfvars"}, // fallback: file exists on disk
	}
	if r.TF == nil {
		t.Fatal("no terraform finding")
	}
	got := *r.TF
	if got.Dir != want.Dir || !reflect.DeepEqual(got.VarFiles, want.VarFiles) || !reflect.DeepEqual(got.BackendConfig, want.BackendConfig) {
		t.Errorf("tf = %+v, want %+v", got, want)
	}
	if !strings.Contains(got.Source, "TerraformCLI@0") {
		t.Errorf("source %q should point at the plan step", got.Source)
	}
}

func TestFindPipelines(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{
		"azure-pipelines.yml", "sub/azure-pipelines-dev.yaml", "pipelines/a.yml", "pipelines/templates/b.yml",
		".azure-pipelines/c.yml", "other/d.yml", "node_modules/x/azure-pipelines.yml", ".venv/pipelines/e.yml", "docs/pipelines.md",
	} {
		p := filepath.Join(root, f)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte("steps: []\n"), 0o644)
	}
	var got []string
	for _, p := range FindPipelines(root) {
		got = append(got, filepath.ToSlash(rel(root, p)))
	}
	sort.Strings(got)
	want := []string{".azure-pipelines/c.yml", "azure-pipelines.yml", "pipelines/a.yml", "pipelines/templates/b.yml", "sub/azure-pipelines-dev.yaml"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestFilterEnv(t *testing.T) {
	files := []string{"p/azure-pipelines-dev.yml", "p/azure-pipelines-prod.yml", "p/azure-pipelines-test.yml", "p/acr-self-grant.yml"}
	got := filterEnv(files, "dev")
	want := []string{"p/azure-pipelines-dev.yml", "p/acr-self-grant.yml"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q want %q", got, want)
	}
	if got := filterEnv(files[1:3], "dev"); len(got) != 2 {
		t.Errorf("no env match must fall back to all files, got %q", got)
	}
}

func TestSub(t *testing.T) {
	c := &scope{
		params: map[string]string{"env": "dev"},
		vars:   map[string]string{"a": "$(b)-${{ parameters.env }}", "b": "x", "loop": "$(loop)"},
	}
	for in, want := range map[string]string{
		"$(a)":                             "x-dev",
		"$(missing)/$(a)":                  "$(missing)/x-dev",
		"${{ variables.b }}":               "x",
		"$(loop)":                          "$(loop)",
		"$(Build.SourcesDirectory)/apps/x": "apps/x",
	} {
		if got := c.sub(in); got != want {
			t.Errorf("sub(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- contoso (fictional reference project) ---

func TestContosoCodePipeline(t *testing.T) {
	r := scan(t, "testdata/contoso/code")
	if len(r.Pipelines) != 1 || !strings.HasSuffix(r.Pipelines[0], "azure-pipelines-dev.yml") {
		t.Fatalf("pipelines = %v, want only the dev one", r.Pipelines)
	}
	got := tuples(r.Findings)
	want := []string{
		"eund-app-contoso-backend|apps/web-backend|Dockerfile|contoso-web-backend|docker",
		"eund-app-contoso-frontend|apps/web-frontend|Dockerfile|contoso-web-frontend|docker",
		"eund-func-billing|apps/func-billing|||zip",
		"eund-func-catalog|apps/func-catalog|||zip",
		"eund-func-media|apps/func-media|||zip",
		"eund-func-orders|apps/func-orders|||zip",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("findings\n got %q\nwant %q", got, want)
	}
	for _, f := range r.Findings {
		if f.Confidence != "medium" || !strings.HasPrefix(f.Source, "pipelines/azure-pipelines-dev.yml:") {
			t.Errorf("%s: source/confidence %q %q", f.Path, f.Source, f.Confidence)
		}
		if f.Image != "" && f.Port == 0 {
			t.Errorf("%s: WEBSITES_PORT not picked up", f.Path)
		}
	}
	if r.TF != nil {
		t.Errorf("code repo has no terraform, got %+v", r.TF)
	}
}

func TestContosoInfraPipeline(t *testing.T) {
	r := scan(t, "testdata/contoso/infra")
	if r.TF == nil {
		t.Fatal("no terraform finding")
	}
	if r.TF.Dir != "terraform" {
		t.Errorf("dir = %q", r.TF.Dir)
	}
	if want := []string{"configs/_tenant.tfvars", "configs/{env}/terraform.tfvars"}; !reflect.DeepEqual(r.TF.VarFiles, want) {
		t.Errorf("var_files = %q, want %q", r.TF.VarFiles, want)
	}
	if want := []string{"configs/{env}/backend.tfvars"}; !reflect.DeepEqual(r.TF.BackendConfig, want) {
		t.Errorf("backend_config = %q, want %q", r.TF.BackendConfig, want)
	}
	if len(r.Pipelines) != 1 {
		t.Errorf("templates must not be scanned standalone: %v", r.Pipelines)
	}
}

// contosoModel mirrors what infra.Plan yields for the reference project.
func contosoModel() *infra.Model {
	zip := func(n string) infra.App {
		return infra.App{Name: n, Host: "functions", Kind: "zip", Runtime: "python", RuntimeVersion: "3.12"}
	}
	dock := func(n, img string) infra.App {
		return infra.App{Name: n, Host: "appservice", Kind: "docker", DockerImage: img, AppSettings: map[string]string{}}
	}
	return &infra.Model{Env: "dev", Apps: []infra.App{
		zip("eund-func-orders"), zip("eund-func-billing"), zip("eund-func-media"), zip("eund-func-catalog"),
		dock("eund-app-contoso-backend", "contoso-web-backend:latest"),
		dock("eund-app-contoso-frontend", "contoso-web-frontend:latest"),
	}}
}

func matchedPairs(mr MatchResult) map[string]string {
	out := map[string]string{}
	for _, m := range mr.Matched {
		out[m.Path] = m.App.Name + " via " + m.How
	}
	return out
}

func TestMatchContoso(t *testing.T) {
	r := scan(t, "testdata/contoso/code")
	mr := Match(r.Findings, contosoModel())
	want := map[string]string{
		"apps/func-orders":  "eund-func-orders via name",
		"apps/func-billing": "eund-func-billing via name",
		"apps/func-media":   "eund-func-media via name",
		"apps/func-catalog": "eund-func-catalog via name",
		"apps/web-backend":  "eund-app-contoso-backend via name",
		"apps/web-frontend": "eund-app-contoso-frontend via name",
	}
	if got := matchedPairs(mr); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
	if len(mr.Unmatched) != 0 || len(mr.Missing) != 0 {
		t.Errorf("unmatched=%v missing=%v", mr.Unmatched, mr.Missing)
	}
}

// Same project when the app names live in a variable group: only paths and image repos remain.
func TestMatchContosoUnresolvedNames(t *testing.T) {
	r := scan(t, "testdata/contoso/code")
	var fs []Finding
	for _, f := range r.Findings {
		f.AppName = "$(someGroupVariable)"
		f.Confidence = "low"
		fs = append(fs, f)
	}
	mr := Match(fs, contosoModel())
	want := map[string]string{
		"apps/func-orders":  "eund-func-orders via tokens",
		"apps/func-billing": "eund-func-billing via tokens",
		"apps/func-media":   "eund-func-media via tokens",
		"apps/func-catalog": "eund-func-catalog via tokens",
		"apps/web-backend":  "eund-app-contoso-backend via image",
		"apps/web-frontend": "eund-app-contoso-frontend via image",
	}
	if got := matchedPairs(mr); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
}

func TestMatchEdgeCases(t *testing.T) {
	m := &infra.Model{Apps: []infra.App{
		{Name: "eund-func-alpha", Kind: "zip"},
		{Name: "eund-func-beta", Kind: "zip"},
		{Name: "eund-func-gamma", Kind: "zip"},
	}}
	fs := []Finding{
		{Path: "apps/func-alpha", Kind: "zip"},                    // unique token overlap
		{Path: "apps/worker"},                                     // no overlap
		{Path: "apps/func", Kind: "zip"},                          // "func" is common to all apps: dropped, no tokens left
		{Path: "apps/alpha-beta", Kind: "zip"},                    // tie between alpha and beta
		{Path: "apps/x", AppName: "eund-func-gamma", Kind: "zip"}, // exact name
	}
	mr := Match(fs, m)
	got := matchedPairs(mr)
	want := map[string]string{"apps/func-alpha": "eund-func-alpha via tokens", "apps/x": "eund-func-gamma via name"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("matched %v, want %v", got, want)
	}
	if len(mr.Unmatched) != 3 {
		t.Errorf("unmatched = %d, want 3", len(mr.Unmatched))
	}
	if len(mr.Missing) != 1 || mr.Missing[0].Name != "eund-func-beta" {
		t.Errorf("missing = %v", mr.Missing)
	}
	if mr := Match(fs, nil); len(mr.Unmatched) != len(fs) {
		t.Errorf("nil model must leave everything unmatched")
	}
}

func TestDraftRenderAndMerge(t *testing.T) {
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, "apps/web-backend"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "apps/web-backend/Dockerfile"), []byte("FROM x\nEXPOSE 9000\nEXPOSE 1\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(root, "apps/web-frontend"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "apps/web-frontend/Dockerfile"), []byte("FROM x\nEXPOSE 80\n"), 0o644)

	m := contosoModel()
	m.Apps = append(m.Apps, infra.App{Name: "eund-func-orphan", Kind: "zip"})
	m.Apps[4].AppSettings = map[string]string{"WEBSITES_PORT": "8000"}
	r := scan(t, "testdata/contoso/code")
	inf := config.Infra{Dir: "../infra/terraform", VarFiles: []string{"configs/_tenant.tfvars"}}
	d := Draft(root, "dev", inf, Match(r.Findings, m))
	b, err := d.Render()
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, s := range []string{
		"# from pipelines/azure-pipelines-dev.yml:", "(medium)", "# TODO: eund-func-orphan",
	} {
		if !strings.Contains(text, s) {
			t.Errorf("rendered yaml lacks %q:\n%s", s, text)
		}
	}
	var c config.Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	w := c.Apps["func-orders"]
	if w == nil || w.Path != "apps/func-orders" || w.Kind != "zip" || w.Runtime != "python:3.12" || w.Resource["dev"] != "eund-func-orders" {
		t.Errorf("func-orders = %+v", w)
	}
	be, fe := c.Apps["web-backend"], c.Apps["web-frontend"]
	if be.Kind != "docker" || be.Dockerfile != "Dockerfile" || be.Port != 8000 { // model WEBSITES_PORT beats EXPOSE
		t.Errorf("web-backend = %+v", be)
	}
	if fe.Port != 80 {
		t.Errorf("web-frontend port = %d, want 80", fe.Port)
	}
	if c.Infra.Dir != "../infra/terraform" || len(c.Apps) != 6 {
		t.Errorf("infra/apps = %+v / %d", c.Infra, len(c.Apps))
	}

	// --update: keep manual edits and comments, add the new env's resource and new apps.
	existing := `env: dev
infra:
  dir: ../infra/terraform
apps:
  # my note
  func-orders:
    path: apps/func-orders
    resource: {test: eund-func-orders-t}
    kind: zip
    env: {MANUAL: "1"}
`
	b2, err := d.MergeInto([]byte(existing), "dev")
	if err != nil {
		t.Fatal(err)
	}
	var c2 config.Config
	if err := yaml.Unmarshal(b2, &c2); err != nil {
		t.Fatal(err)
	}
	w2 := c2.Apps["func-orders"]
	if w2.Env["MANUAL"] != "1" || w2.Resource["test"] != "eund-func-orders-t" || w2.Resource["dev"] != "eund-func-orders" {
		t.Errorf("merged func-orders = %+v", w2)
	}
	if len(c2.Apps) != 6 || !strings.Contains(string(b2), "# my note") || !strings.Contains(string(b2), "# from pipelines/") {
		t.Errorf("merge result:\n%s", b2)
	}
}

func TestMergeIntoAddsMissingRun(t *testing.T) {
	d := &Proposal{Config: &config.Config{Apps: map[string]*config.App{
		"web": {Path: "apps/web", Kind: "docker", Run: "npm run dev -- --port {port}", HostHeader: "local"},
		"api": {Path: "apps/api", Kind: "docker", Run: "derived"},
	}}}
	existing := "env: dev\napps:\n  web:\n    path: apps/web\n    kind: docker\n  api:\n    path: apps/api\n    kind: docker\n    run: mine\n"
	b, err := d.MergeInto([]byte(existing), "dev")
	if err != nil {
		t.Fatal(err)
	}
	var c config.Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	if w := c.Apps["web"]; w.Run != "npm run dev -- --port {port}" || w.HostHeader != "local" {
		t.Errorf("web = %+v", w)
	}
	if c.Apps["api"].Run != "mine" {
		t.Errorf("an existing run must be kept, got %q", c.Apps["api"].Run)
	}
}

func TestDraftUnmatched(t *testing.T) {
	d := Draft(t.TempDir(), "dev", config.Infra{}, MatchResult{Unmatched: []Finding{
		{Path: "apps/known", AppName: "known-dev", Kind: "zip", Source: "p.yml:1 x", Confidence: "medium"},
		{Path: "apps/nameless", AppName: "$(grp)", Kind: "zip", Source: "p.yml:2 x", Confidence: "low"},
		{Image: "pathless-image", Kind: "docker"},
	}})
	if len(d.Config.Apps) != 2 || d.Config.Apps["known"].Resource["dev"] != "known-dev" || len(d.Config.Apps["nameless"].Resource) != 0 {
		t.Errorf("apps = %+v", d.Config.Apps)
	}
	b, _ := d.Render()
	if !strings.Contains(string(b), "UNMATCHED") {
		t.Errorf("unmatched apps must be flagged:\n%s", b)
	}
}

func TestResolveInfra(t *testing.T) {
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, ".git"), 0o755)
	_ = os.MkdirAll(filepath.Join(root, "infra/terraform"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "infra/terraform/main.tf"), nil, 0o644)
	gotRoot, gotTF := ResolveInfra(filepath.Join(root, "infra/terraform"))
	if r, _ := filepath.EvalSymlinks(gotRoot); r != mustEval(t, root) || filepath.Base(gotTF) != "terraform" {
		t.Errorf("tf dir: root=%s tf=%s", gotRoot, gotTF)
	}
	gotRoot, gotTF = ResolveInfra(root)
	if filepath.Base(gotRoot) != filepath.Base(root) || gotTF != "" {
		t.Errorf("repo root: root=%s tf=%s", gotRoot, gotTF)
	}
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

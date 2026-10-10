package infra

// Corpus test: real public azurerm projects under testdata/corpus/<name>/ are the oracle for
// the static evaluator. Per project:
//
//	expected.json  what azemu must extract (written by hand from the HCL)
//	plan.json      optional, slimmed `terraform show -json` of a real plan (second oracle)
//
// expected.json value conventions (apps[].settings, names, storage accounts, vaults):
//
//	"text"      exact match
//	"re:<expr>" regexp match (names derived from azurecaf / subscription id, URLs with unknown hosts)
//	null        settings only: the key must exist, its value is Azure-assigned or secret
//
// The app/storage/vault lists are complete: anything the model has beyond them is an error.
// Projects marked "needs_network" run `terraform init` for registry modules; set
// AZEMU_CORPUS_OFFLINE=1 to skip them.
//
// testdata/corpus/*.local/ is gitignored: private projects. Next to its expected.json, a
// source.json {"dir": "/abs/terraform/root", "var_files": ["..."]} says where the project lives;
// it overrides "dir" and "var_files" of expected.json, and the project is skipped when that path
// is absent.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/luigi23997/azemu/internal/infra/tfstatic"
)

type expApp struct {
	Name           string             `json:"name"`
	ResourceType   string             `json:"resource_type"`
	Host           string             `json:"host"`
	Kind           string             `json:"kind"`
	Runtime        string             `json:"runtime"`
	RuntimeVersion string             `json:"runtime_version"`
	DockerImage    string             `json:"docker_image"`
	HostStorage    string             `json:"host_storage"`
	HostStorageMI  bool               `json:"host_storage_mi"`
	EasyAuth       bool               `json:"easy_auth"`
	CommandLine    string             `json:"command_line"`
	Settings       map[string]*string `json:"settings"`
}

type expStorage struct {
	Name       string   `json:"name"`
	Containers []string `json:"containers"`
	Tables     []string `json:"tables"`
	Queues     []string `json:"queues"`
}

type expected struct {
	Dir             string       `json:"dir"`       // terraform root, relative to the project dir (default ".") or absolute
	VarFiles        []string     `json:"var_files"` // relative to Dir (default ["terraform.tfvars"])
	PlanFile        string       `json:"plan_file"` // relative to the project dir (default "plan.json" if present)
	Env             string       `json:"env"`
	NeedsNetwork    bool         `json:"needs_network"`
	Apps            []expApp     `json:"apps"`
	StorageAccounts []expStorage `json:"storage_accounts"`
	Vaults          []string     `json:"vaults"`
	WarningsContain []string     `json:"warnings_contain"`
}

func match(exp, got string) bool {
	if re, ok := strings.CutPrefix(exp, "re:"); ok {
		return regexp.MustCompile(re).MatchString(got)
	}
	return exp == got
}

// compareExpected returns one line per disagreement between the model and the oracle.
func compareExpected(e *expected, m *Model) []string {
	var p []string
	bad := func(f string, a ...any) { p = append(p, fmt.Sprintf(f, a...)) }

	claimed := map[int]bool{}
	for _, ea := range e.Apps {
		idx := -1
		for i := range m.Apps {
			if !claimed[i] && match(ea.Name, m.Apps[i].Name) {
				idx = i
				break
			}
		}
		if idx < 0 {
			bad("app %s: missing from the model", ea.Name)
			continue
		}
		claimed[idx] = true
		a := &m.Apps[idx]
		w := "app " + a.Name + ": "
		for _, f := range []struct{ n, exp, got string }{
			{"resource_type", ea.ResourceType, a.ResourceType}, {"host", ea.Host, a.Host}, {"kind", ea.Kind, a.Kind},
			{"runtime", ea.Runtime, a.Runtime}, {"runtime_version", ea.RuntimeVersion, a.RuntimeVersion},
			{"docker_image", ea.DockerImage, a.DockerImage}, {"host_storage", ea.HostStorage, a.HostStorage},
			{"command_line", ea.CommandLine, a.CommandLine},
		} {
			if !match(f.exp, f.got) {
				bad("%s%s = %q, want %q", w, f.n, f.got, f.exp)
			}
		}
		if a.HostStorageMI != ea.HostStorageMI {
			bad("%shost_storage_mi = %v, want %v", w, a.HostStorageMI, ea.HostStorageMI)
		}
		if a.EasyAuth != ea.EasyAuth {
			bad("%seasy_auth = %v, want %v", w, a.EasyAuth, ea.EasyAuth)
		}

		unknown := map[string]bool{}
		for _, k := range a.UnknownSettings {
			unknown[k] = true
		}
		if unknown["*"] {
			bad("%sall app_settings unknown", w)
			continue
		}
		var keys []string
		for k := range ea.Settings {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v, ok := a.AppSettings[k]
			switch {
			case !ok && unknown[k] && ea.Settings[k] == nil: // Azure-assigned: unknown is the right answer
			case !ok && unknown[k]:
				bad("%ssetting %s: unknown, want %q", w, k, *ea.Settings[k])
			case !ok:
				bad("%ssetting %s: missing", w, k)
			case ea.Settings[k] != nil && !match(*ea.Settings[k], v):
				bad("%ssetting %s = %q, want %q", w, k, maskValue(k, v), maskValue(k, *ea.Settings[k]))
			}
		}
		for k := range a.AppSettings {
			if _, ok := ea.Settings[k]; !ok {
				bad("%ssetting %s: unexpected", w, k)
			}
		}
		for k := range unknown {
			if _, ok := ea.Settings[k]; !ok {
				bad("%ssetting %s: unexpected (unknown)", w, k)
			}
		}
	}
	for i, a := range m.Apps {
		if !claimed[i] {
			bad("app %s: unexpected", a.Name)
		}
	}

	claimedS := map[int]bool{}
	for _, es := range e.StorageAccounts {
		idx := -1
		for i := range m.StorageAccounts {
			if !claimedS[i] && match(es.Name, m.StorageAccounts[i].Name) {
				idx = i
				break
			}
		}
		if idx < 0 {
			bad("storage account %s: missing from the model", es.Name)
			continue
		}
		claimedS[idx] = true
		s := m.StorageAccounts[idx]
		for _, f := range []struct {
			n        string
			exp, got []string
		}{{"containers", es.Containers, s.Containers}, {"tables", es.Tables, s.Tables}, {"queues", es.Queues, s.Queues}} {
			a, b := setDiff(f.got, f.exp)
			if len(a)+len(b) > 0 {
				bad("storage account %s: %s = %v, want %v", s.Name, f.n, f.got, f.exp)
			}
		}
	}
	for i, s := range m.StorageAccounts {
		if !claimedS[i] {
			bad("storage account %s: unexpected", s.Name)
		}
	}

	claimedV := map[int]bool{}
	for _, ev := range e.Vaults {
		found := false
		for i, v := range m.Vaults {
			if !claimedV[i] && match(ev, v) {
				claimedV[i], found = true, true
				break
			}
		}
		if !found {
			bad("vault %s: missing from the model", ev)
		}
	}
	for i, v := range m.Vaults {
		if !claimedV[i] {
			bad("vault %s: unexpected", v)
		}
	}

	for _, w := range e.WarningsContain {
		found := false
		for _, mw := range m.Warnings {
			if strings.Contains(mw, w) {
				found = true
				break
			}
		}
		if !found {
			bad("no warning mentions %q", w)
		}
	}
	return p
}

// maskValue keeps secrets out of test logs.
func maskValue(k, v string) string {
	u := strings.ToUpper(k)
	for _, s := range []string{"KEY", "SECRET", "TOKEN", "PASSWORD", "CONNECTION"} {
		if strings.Contains(u, s) && !strings.Contains(v, "@Microsoft.KeyVault") && !strings.HasPrefix(v, "re:") && len(v) > 4 {
			return v[:4] + "..."
		}
	}
	return v
}

type corpusProject struct {
	name, dir, planFile string
	e                   expected
}

func loadCorpus(t *testing.T) []corpusProject {
	root := filepath.FromSlash("../../testdata/corpus")
	ents, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var ps []corpusProject
	for _, d := range ents {
		pd := filepath.Join(root, d.Name())
		b, err := os.ReadFile(filepath.Join(pd, "expected.json"))
		if err != nil {
			continue
		}
		p := corpusProject{name: d.Name()}
		if err := json.Unmarshal(b, &p.e); err != nil {
			t.Fatalf("%s/expected.json: %v", d.Name(), err)
		}
		if sb, err := os.ReadFile(filepath.Join(pd, "source.json")); err == nil {
			var src struct {
				Dir      string   `json:"dir"`
				VarFiles []string `json:"var_files"`
			}
			if err := json.Unmarshal(sb, &src); err != nil {
				t.Fatalf("%s/source.json: %v", d.Name(), err)
			}
			p.e.Dir, p.e.VarFiles = src.Dir, src.VarFiles
		}
		p.dir = p.e.Dir
		if p.dir == "" {
			p.dir = "."
		}
		if !filepath.IsAbs(p.dir) {
			p.dir = filepath.Join(pd, p.dir)
		}
		if p.dir, err = filepath.Abs(p.dir); err != nil {
			t.Fatal(err)
		}
		if len(p.e.VarFiles) == 0 {
			p.e.VarFiles = []string{"terraform.tfvars"}
		}
		if p.e.Env == "" {
			p.e.Env = "corpus"
		}
		pf := p.e.PlanFile
		if pf == "" {
			pf = "plan.json"
		}
		if full := filepath.Join(pd, pf); fileExists(full) {
			p.planFile = full
		}
		ps = append(ps, p)
	}
	return ps
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func summary(m *Model) string {
	return fmt.Sprintf("%d apps, %d storage accounts, %d vaults, %d warnings", len(m.Apps), len(m.StorageAccounts), len(m.Vaults), len(m.Warnings))
}

// TestCorpus runs the static evaluator on every corpus project and compares the model with
// expected.json and, when present, with the real terraform plan (plan.json). It fails until the
// evaluator handles the constructs listed in the corpus README.
func TestCorpus(t *testing.T) {
	for _, p := range loadCorpus(t) {
		t.Run(p.name, func(t *testing.T) {
			if !fileExists(p.dir) {
				t.Skipf("%s not present", p.dir)
			}
			if p.e.NeedsNetwork && os.Getenv("AZEMU_CORPUS_OFFLINE") != "" {
				t.Skip("needs network (AZEMU_CORPUS_OFFLINE set)")
			}
			js, warns, err := tfstatic.Evaluate(t.Context(), tfstatic.Options{Dir: p.dir, VarFiles: p.e.VarFiles, DataDir: t.TempDir()})
			if err != nil {
				t.Fatalf("static evaluation failed: %v", err)
			}
			m, err := FromPlanJSON(js, p.e.Env)
			if err != nil {
				t.Fatalf("static plan json: %v", err)
			}
			m.Warnings = append(warns, m.Warnings...)
			t.Logf("static model: %s", summary(m))

			problems := compareExpected(&p.e, m)
			report(t, "expected.json", problems)

			if p.planFile != "" {
				b, err := os.ReadFile(p.planFile)
				if err != nil {
					t.Fatal(err)
				}
				pm, err := FromPlanJSON(b, p.e.Env)
				if err != nil {
					t.Fatalf("%s: %v", p.planFile, err)
				}
				res := Diff(m, pm)
				t.Logf("vs plan: compared %d, skipped %d (unknown in plan), %d mismatches", res.Compared, res.Skipped, len(res.Mismatches))
				var lines []string
				for _, mm := range res.Mismatches {
					lines = append(lines, mm.String())
				}
				report(t, "plan", lines)
			}
		})
	}
}

func report(t *testing.T, oracle string, problems []string) {
	t.Helper()
	if len(problems) == 0 {
		t.Logf("%s: OK", oracle)
		return
	}
	const limit = 60
	for i, s := range problems {
		if i == limit {
			t.Errorf("%s: ... and %d more", oracle, len(problems)-limit)
			break
		}
		t.Errorf("%s: %s", oracle, s)
	}
}

// TestCorpusOraclesAgree checks, with no evaluator involved, that every plan.json agrees with
// the hand-written expected.json wherever the plan knows a value, so a wrong oracle is caught
// before it blames the evaluator.
func TestCorpusOraclesAgree(t *testing.T) {
	for _, p := range loadCorpus(t) {
		if p.planFile == "" {
			continue
		}
		t.Run(p.name, func(t *testing.T) {
			b, err := os.ReadFile(p.planFile)
			if err != nil {
				t.Fatal(err)
			}
			pm, err := FromPlanJSON(b, p.e.Env)
			if err != nil {
				t.Fatal(err)
			}
			for _, ea := range p.e.Apps {
				a := pm.App(ea.Name)
				if a == nil {
					t.Errorf("expected app %s is not in plan.json", ea.Name)
					continue
				}
				for _, f := range []struct{ n, exp, got string }{
					{"resource_type", ea.ResourceType, a.ResourceType}, {"kind", ea.Kind, a.Kind},
					{"runtime", ea.Runtime, a.Runtime}, {"runtime_version", ea.RuntimeVersion, a.RuntimeVersion},
					{"docker_image", ea.DockerImage, a.DockerImage}, {"host_storage", ea.HostStorage, a.HostStorage},
				} {
					if f.got != "" && !match(f.exp, f.got) {
						t.Errorf("%s %s: expected.json %q, plan %q", ea.Name, f.n, f.exp, f.got)
					}
				}
				if a.EasyAuth != ea.EasyAuth || a.HostStorageMI != ea.HostStorageMI {
					t.Errorf("%s: easy_auth/host_storage_mi expected %v/%v, plan %v/%v", ea.Name, ea.EasyAuth, ea.HostStorageMI, a.EasyAuth, a.HostStorageMI)
				}
				for k, v := range a.AppSettings {
					exp, ok := ea.Settings[k]
					switch {
					case !ok:
						t.Errorf("%s setting %s: plan has it, expected.json does not", ea.Name, k)
					case exp != nil && !match(*exp, v):
						t.Errorf("%s setting %s: expected.json %q, plan %q", ea.Name, k, *exp, v)
					}
				}
				if len(a.UnknownSettings) == 0 || a.UnknownSettings[0] != "*" {
					for k := range ea.Settings {
						if _, ok := a.AppSettings[k]; !ok && !slices.Contains(a.UnknownSettings, k) {
							t.Errorf("%s setting %s: expected.json has it, plan does not", ea.Name, k)
						}
					}
				}
			}
			for _, es := range p.e.StorageAccounts {
				found := false
				for _, s := range pm.StorageAccounts {
					if match(es.Name, s.Name) {
						found = true
						// the plan drops containers whose account id is unknown, so only extras are wrong
						if x, _ := setDiff(s.Containers, es.Containers); len(x) > 0 {
							t.Errorf("storage %s containers: plan %v, expected.json %v", es.Name, s.Containers, es.Containers)
						}
					}
				}
				if !found {
					t.Errorf("expected storage account %s is not in plan.json", es.Name)
				}
			}
		})
	}
}

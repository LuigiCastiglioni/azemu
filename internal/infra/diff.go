package infra

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Mismatch is one disagreement between a static model and a plan model.
type Mismatch struct {
	Scope  string // "app NAME", "storage NAME", "vaults"
	Field  string // "kind", "setting KEY", "exists", ...
	Kind   string // only-static | only-plan | value | missing | extra | static-unknown
	Static string
	Plan   string
}

func (m Mismatch) String() string {
	return fmt.Sprintf("%-28s %-34s %-14s static=%q plan=%q", m.Scope, m.Field, m.Kind, m.Static, m.Plan)
}

// DiffResult is the outcome of Diff. Skipped counts comparisons dropped because the plan could
// not tell (value unknown until apply) -- those are not mismatches.
type DiffResult struct {
	Mismatches []Mismatch
	Compared   int
	Skipped    int
}

// Diff compares what the static evaluator extracted with what terraform plan reported.
// Rule: the plan is the oracle, but only where it knows the value. Empty plan strings and
// plan-unknown settings are skipped; plan entities without a name (name unknown until apply)
// cannot be matched, so they excuse static-only entities of the same kind.
func Diff(static, plan *Model) DiffResult {
	var r DiffResult
	add := func(scope, field, kind, s, p string) {
		r.Mismatches = append(r.Mismatches, Mismatch{scope, field, kind, s, p})
	}

	// --- apps
	sApps, pApps := map[string]*App{}, map[string]*App{}
	var sNames, pNames []string
	planUnnamed := 0
	for i := range static.Apps {
		sApps[static.Apps[i].Name] = &static.Apps[i]
		sNames = append(sNames, static.Apps[i].Name)
	}
	for i := range plan.Apps {
		if plan.Apps[i].Name == "" {
			planUnnamed++
			continue
		}
		pApps[plan.Apps[i].Name] = &plan.Apps[i]
		pNames = append(pNames, plan.Apps[i].Name)
	}
	onlyS, onlyP := setDiff(sNames, pNames)
	for _, n := range onlyP {
		add("app "+n, "exists", "only-plan", "", n)
	}
	for _, n := range onlyS {
		if planUnnamed > 0 {
			r.Skipped++ // may be one of the plan apps whose name is unknown
			continue
		}
		add("app "+n, "exists", "only-static", n, "")
	}
	for _, n := range sNames {
		if p := pApps[n]; p != nil {
			diffApp("app "+n, sApps[n], p, &r, add)
		}
	}

	// --- storage accounts
	// The plan model drops containers/tables/queues whose account or name is unknown until
	// apply (it records a warning), so static-only ones are then not provably wrong.
	planSkipsObjects := false
	for _, w := range plan.Warnings {
		if strings.Contains(w, "unknown until apply, skipped") {
			planSkipsObjects = true
		}
	}
	sAcc, pAcc := map[string]StorageAccount{}, map[string]StorageAccount{}
	var sAccN, pAccN []string
	accUnnamed := 0
	for _, a := range static.StorageAccounts {
		sAcc[a.Name] = a
		sAccN = append(sAccN, a.Name)
	}
	for _, a := range plan.StorageAccounts {
		if a.Name == "" {
			accUnnamed++
			continue
		}
		pAcc[a.Name] = a
		pAccN = append(pAccN, a.Name)
	}
	onlyS, onlyP = setDiff(sAccN, pAccN)
	for _, n := range onlyP {
		add("storage "+n, "exists", "only-plan", "", n)
	}
	for _, n := range onlyS {
		if accUnnamed > 0 {
			r.Skipped++
			continue
		}
		add("storage "+n, "exists", "only-static", n, "")
	}
	for _, n := range sAccN {
		p, ok := pAcc[n]
		if !ok {
			continue
		}
		s := sAcc[n]
		for _, f := range []struct {
			name string
			s, p []string
		}{{"containers", s.Containers, p.Containers}, {"tables", s.Tables, p.Tables}, {"queues", s.Queues, p.Queues}} {
			r.Compared++
			a, b := setDiff(f.s, f.p)
			for _, x := range b {
				add("storage "+n, f.name, "only-plan", "", x)
			}
			for _, x := range a {
				if planSkipsObjects {
					r.Skipped++
					continue
				}
				add("storage "+n, f.name, "only-static", x, "")
			}
		}
	}

	// --- vaults (azurerm_key_vault resources only; unknown names are not in the model)
	r.Compared++
	a, b := setDiff(static.Vaults, plan.Vaults)
	for _, x := range b {
		add("vaults", "exists", "only-plan", "", x)
	}
	for _, x := range a {
		add("vaults", "exists", "only-static", x, "")
	}

	sort.SliceStable(r.Mismatches, func(i, j int) bool {
		x, y := r.Mismatches[i], r.Mismatches[j]
		return x.Scope+"\x00"+x.Field < y.Scope+"\x00"+y.Field
	})
	return r
}

func diffApp(scope string, s, p *App, r *DiffResult, add func(scope, field, kind, s, p string)) {
	str := func(field, sv, pv string) {
		if pv == "" { // unset or unknown in the plan: nothing to compare against
			r.Skipped++
			return
		}
		r.Compared++
		if sv != pv {
			add(scope, field, "value", sv, pv)
		}
	}
	str("resource_type", s.ResourceType, p.ResourceType)
	str("host", s.Host, p.Host)
	str("kind", s.Kind, p.Kind)
	str("runtime", s.Runtime, p.Runtime)
	str("runtime_version", s.RuntimeVersion, p.RuntimeVersion)
	str("docker_image", s.DockerImage, p.DockerImage)
	str("host_storage", s.HostStorage, p.HostStorage)
	str("command_line", s.CommandLine, p.CommandLine)
	for _, b := range []struct {
		field string
		s, p  bool
	}{{"easy_auth", s.EasyAuth, p.EasyAuth}, {"host_storage_mi", s.HostStorageMI, p.HostStorageMI}} {
		r.Compared++
		if b.s != b.p {
			add(scope, b.field, "value", fmt.Sprint(b.s), fmt.Sprint(b.p))
		}
	}

	// app_settings
	if slices.Contains(p.UnknownSettings, "*") { // the whole map is unknown until apply
		r.Skipped++
		return
	}
	pUnk := p.UnknownSettings
	sUnk := s.UnknownSettings
	if slices.Contains(sUnk, "*") {
		add(scope, "app_settings", "static-unknown", "*", fmt.Sprintf("%d keys", len(p.AppSettings)))
		return
	}
	keys := map[string]bool{}
	for k := range s.AppSettings {
		keys[k] = true
	}
	for k := range p.AppSettings {
		keys[k] = true
	}
	for _, k := range sUnk {
		keys[k] = true
	}
	for _, k := range pUnk {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	for _, k := range sorted {
		sv, sIn := s.AppSettings[k]
		pv, pIn := p.AppSettings[k]
		sIsUnk, pIsUnk := slices.Contains(sUnk, k), slices.Contains(pUnk, k)
		field := "setting " + k
		switch {
		case pIsUnk: // plan cannot tell: skip whatever static has
			r.Skipped++
		case sIsUnk && pIn:
			r.Compared++
			add(scope, field, "static-unknown", "", pv)
		case sIsUnk: // unknown in static and absent in plan: nothing to say
			r.Skipped++
		case pIn && !sIn:
			r.Compared++
			add(scope, field, "missing", "", pv)
		case sIn && !pIn:
			r.Compared++
			add(scope, field, "extra", sv, "")
		case sv != pv:
			r.Compared++
			add(scope, field, "value", sv, pv)
		default:
			r.Compared++
		}
	}
}

// setDiff returns the elements only in a and only in b (both sorted, deduplicated).
func setDiff(a, b []string) (onlyA, onlyB []string) {
	in := func(l []string) map[string]bool {
		m := map[string]bool{}
		for _, x := range l {
			m[x] = true
		}
		return m
	}
	ma, mb := in(a), in(b)
	for x := range ma {
		if !mb[x] {
			onlyA = append(onlyA, x)
		}
	}
	for x := range mb {
		if !ma[x] {
			onlyB = append(onlyB, x)
		}
	}
	sort.Strings(onlyA)
	sort.Strings(onlyB)
	return
}

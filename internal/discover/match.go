package discover

import (
	"path"
	"sort"

	"github.com/luigi23997/azemu/internal/infra"
)

// Matched joins a finding to a model app. How is "name", "image" or "tokens".
type Matched struct {
	Finding
	App *infra.App
	How string
}

type MatchResult struct {
	Matched   []Matched
	Unmatched []Finding   // findings with no (or an ambiguous) model app
	Missing   []infra.App // model apps no finding points to
}

// Match joins findings to m.Apps: exact name, then image repo, then token overlap
// between folder/image and app name. Ties and zero overlap stay unmatched.
// ponytail: no interactive disambiguation; ambiguous findings become commented TODOs.
func Match(fs []Finding, m *infra.Model) MatchResult {
	var r MatchResult
	fs = append([]Finding(nil), fs...) // confidence is adjusted below
	if m == nil {
		r.Unmatched = fs
		return r
	}
	app := make([]int, len(fs)) // finding -> app index, -1 unmatched
	how := make([]string, len(fs))
	claimed := make([]bool, len(m.Apps))
	for i := range app {
		app[i] = -1
	}
	assign := func(i, j int, h string) { app[i], how[i], claimed[j] = j, h, true }

	for i, f := range fs {
		for j, a := range m.Apps {
			if !claimed[j] && f.AppName != "" && f.AppName == a.Name {
				assign(i, j, "name")
				break
			}
		}
	}
	for i, f := range fs {
		if app[i] >= 0 || f.Image == "" {
			continue
		}
		for j, a := range m.Apps {
			if !claimed[j] && a.DockerImage != "" && f.Image == imageRepo(a.DockerImage) {
				assign(i, j, "image")
				break
			}
		}
	}

	drop := commonTokens(m)
	type cand struct {
		i, j  int
		score float64
	}
	var cands []cand
	for i, f := range fs {
		if app[i] >= 0 {
			continue
		}
		ft := tokenSet(path.Base(f.Path)+" "+path.Base(f.Image), drop)
		best, second, bj := 0.0, 0.0, -1
		for j, a := range m.Apps {
			if claimed[j] {
				continue
			}
			sc := 0.0
			for t := range tokenSet(a.Name, drop) {
				if ft[t] {
					sc++
				}
			}
			if sc == 0 {
				continue
			}
			if f.Kind != "" && f.Kind == a.Kind {
				sc += 0.5
			}
			if sc > best {
				best, second, bj = sc, best, j
			} else if sc > second {
				second = sc
			}
		}
		if bj >= 0 && best > second {
			cands = append(cands, cand{i, bj, best})
		}
	}
	sort.SliceStable(cands, func(a, b int) bool { return cands[a].score > cands[b].score })
	for _, c := range cands {
		if app[c.i] < 0 && !claimed[c.j] {
			assign(c.i, c.j, "tokens")
			if c.score >= 2 {
				fs[c.i].Confidence = capConf(fs[c.i].Confidence, "medium")
			} else {
				fs[c.i].Confidence = "low"
			}
		}
	}

	for i, f := range fs {
		if app[i] < 0 {
			r.Unmatched = append(r.Unmatched, f)
			continue
		}
		r.Matched = append(r.Matched, Matched{Finding: f, App: &m.Apps[app[i]], How: how[i]})
	}
	for j, a := range m.Apps {
		if !claimed[j] {
			r.Missing = append(r.Missing, a)
		}
	}
	return r
}

func capConf(c, max string) string {
	if confRank[c] > confRank[max] {
		return max
	}
	return c
}

// commonTokens are env names plus tokens shared by every app (naming prefixes such as "eund").
func commonTokens(m *infra.Model) map[string]bool {
	drop := map[string]bool{}
	for e := range envNames {
		drop[e] = true
	}
	if len(m.Apps) > 1 {
		cnt := map[string]int{}
		for _, a := range m.Apps {
			for t := range tokenSet(a.Name, nil) {
				cnt[t]++
			}
		}
		for t, n := range cnt {
			if n == len(m.Apps) {
				drop[t] = true
			}
		}
	}
	return drop
}

func tokenSet(s string, drop map[string]bool) map[string]bool {
	out := map[string]bool{}
	for _, t := range splitTokens(s) {
		if len(t) > 1 && !drop[t] {
			out[t] = true
		}
	}
	return out
}

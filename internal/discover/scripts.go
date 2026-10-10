package discover

import (
	"path"
	"regexp"
	"strconv"
	"strings"
)

// Script heuristics (bash + PowerShell). ponytail: regexes over joined lines, not a
// shell parser; quoting/`;` inside quoted strings and control flow are ignored.
var (
	contRe      = regexp.MustCompile("(?:`|\\\\)[ \t]*\r?\n")
	cmdSplitRe  = regexp.MustCompile(`\s*(?:&&|\|\||;)\s*`)
	cdRe        = regexp.MustCompile(`(?i)^(?:cd|set-location|pushd)\s+(?:-(?:literal)?path\s+)?(.+)$`)
	buildRe     = regexp.MustCompile(`(?i)\b(?:docker\s+(?:buildx\s+)?build|az\s+acr\s+build)\s+(.*)$`)
	zipRe       = regexp.MustCompile(`(?i)\baz\s+(?:functionapp|webapp)\s+deployment\s+source\s+config-zip\s+(.*)$`)
	webDeployRe = regexp.MustCompile(`(?i)\baz\s+webapp\s+deploy\s+(.*)$`)
	funcRe      = regexp.MustCompile(`(?i)\bfunc\s+azure\s+functionapp\s+publish\s+(\S+)`)
	tfRe        = regexp.MustCompile(`(?i)\bterraform\s+(plan|apply|init)\b(.*)$`)
	hashRe      = regexp.MustCompile(`(?s)@\{(.*?)\}`)
	kvRe        = regexp.MustCompile(`^\s*([A-Za-z_]\w*)\s*=\s*(.+?)\s*,?\s*$`)
	appNameRe   = regexp.MustCompile(`(?i)-\w*AppName\s+['"]?([^\s'")]+)`)
	appPathRe   = regexp.MustCompile(`(?i)-\w*Path\s+(?:\(Join-Path\s+\S+\s+)?['"]?([^\s'")]+)`)
	imageArgRe  = regexp.MustCompile(`(?i)-\w*Image(?:Repository|Name)\s+['"]?([^\s'"]+)`)
	portArgRe   = regexp.MustCompile(`(?i)-\w*Port\s+['"]?(\d+)`)
	backendRe   = regexp.MustCompile(`[\w./{}-]*backend[\w.-]*\.(?:tfvars|hcl)`)
)

var boolFlags = map[string]bool{
	"--no-cache": true, "--pull": true, "--push": true, "--load": true, "-q": true, "--quiet": true,
	"--no-logs": true, "--rm": true, "--force-rm": true, "--no-wait": true, "--verbose": true,
	"--debug": true, "--only-show-errors": true, "--no-restart": true,
	"--python": true, "--node": true, "--dotnet": true, "--powershell": true, "--java": true,
}

// tokenize splits on whitespace, honouring single/double quotes (stripped).
func tokenize(s string) []string {
	var out []string
	var cur strings.Builder
	var q rune
	has := false
	for _, r := range s {
		switch {
		case q != 0:
			if r == q {
				q = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			q, has = r, true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			if has || cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
				has = false
			}
		default:
			cur.WriteRune(r)
		}
	}
	if has || cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// parseFlags splits tokens into flag->values and positionals.
func parseFlags(toks []string) (map[string][]string, []string) {
	flags := map[string][]string{}
	var pos []string
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if !strings.HasPrefix(t, "-") || t == "-" {
			pos = append(pos, t)
			continue
		}
		if k, v, ok := strings.Cut(t, "="); ok {
			flags[k] = append(flags[k], v)
			continue
		}
		if boolFlags[t] || i+1 >= len(toks) || strings.HasPrefix(toks[i+1], "-") {
			flags[t] = append(flags[t], "")
			continue
		}
		i++
		flags[t] = append(flags[t], toks[i])
	}
	return flags, pos
}

func flag1(flags map[string][]string, names ...string) string {
	for _, n := range names {
		if v := flags[n]; len(v) > 0 && v[0] != "" {
			return v[0]
		}
	}
	return ""
}

// imageRepo reduces "reg.azurecr.io/team/app:tag" to "team/app".
func imageRepo(s string) string {
	s = strings.Trim(strings.TrimSpace(s), `'"`)
	if i := strings.LastIndex(s, ":"); i > strings.LastIndex(s, "/") {
		s = s[:i]
	}
	if reg, rest, ok := strings.Cut(s, "/"); ok && (strings.ContainsAny(reg, ".:") || reg == "localhost") {
		s = rest
	}
	return s
}

// joinPath joins cwd and p; unresolved or absolute values pass through unchanged.
func joinPath(cwd, p string) string {
	p = strings.ReplaceAll(strings.Trim(p, `'"`), `\`, "/")
	if hasUnresolved(p) || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "$") {
		return p
	}
	return normPath(path.Join(cwd, p))
}

// scriptText extracts findings from one script body. wd is the step's workingDirectory.
func (s *scanner) scriptText(text, wd, src, base string) {
	text = contRe.ReplaceAllString(text, " ")
	cwd := "."
	if wd != "" {
		cwd = normPath(wd)
	}
	for _, m := range backendRe.FindAllString(text, -1) {
		if strings.Contains(m, "/") {
			s.tfBackend = append(s.tfBackend, strings.Trim(m, "/"))
		}
	}
	s.hashtables(text, src, base)
	for _, line := range strings.Split(text, "\n") {
		for _, cmd := range cmdSplitRe.Split(strings.TrimSpace(line), -1) {
			cmd = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(cmd), "&"))
			if m := cdRe.FindStringSubmatch(cmd); m != nil {
				if d := strings.Trim(strings.TrimSpace(m[1]), `'"`); !strings.Contains(d, "$") {
					cwd = joinPath(cwd, d)
				}
				continue
			}
			s.command(cmd, cwd, src, base)
		}
	}
}

func (s *scanner) command(cmd, cwd, src, base string) {
	switch {
	case buildRe.MatchString(cmd):
		flags, pos := parseFlags(tokenize(buildRe.FindStringSubmatch(cmd)[1]))
		ctx := "."
		if len(pos) > 0 {
			ctx = pos[len(pos)-1]
		}
		p := joinPath(cwd, ctx)
		f := Finding{Path: p, Image: imageRepo(flag1(flags, "--image", "-t", "--tag")), Kind: "docker"}
		if df := flag1(flags, "--file", "-f"); df != "" {
			f.Dockerfile = strings.TrimPrefix(normPath(df), p+"/")
		}
		s.emit(f, src, base)
	case zipRe.MatchString(cmd) || webDeployRe.MatchString(cmd):
		m := zipRe.FindStringSubmatch(cmd)
		if m == nil {
			m = webDeployRe.FindStringSubmatch(cmd)
		}
		flags, _ := parseFlags(tokenize(m[1]))
		s.emit(Finding{AppName: flag1(flags, "--name", "-n"), pkg: joinPath(cwd, flag1(flags, "--src", "--src-path")), Kind: "zip"}, src, base)
	case funcRe.MatchString(cmd):
		s.emit(Finding{AppName: strings.Trim(funcRe.FindStringSubmatch(cmd)[1], `'"`), Path: cwd, Kind: "zip"}, src, base)
	case tfRe.MatchString(cmd):
		m := tfRe.FindStringSubmatch(cmd)
		s.terraformFlags(strings.ToLower(m[1]), tokenize(m[2]), src)
		if cwd != "." {
			s.setTFDir(cwd, src, strings.EqualFold(m[1], "plan"))
		}
	default:
		s.invocation(cmd, src, base)
	}
}

// invocation handles `script.ps1 -XAppName n -XPath p` / `-XImageRepository r` parameter pairs.
func (s *scanner) invocation(cmd, src, base string) {
	n := appNameRe.FindStringSubmatch(cmd)
	if n == nil || (strings.HasPrefix(n[1], "$") && !strings.HasPrefix(n[1], "$(")) {
		return
	}
	f := Finding{AppName: n[1]}
	if p := appPathRe.FindStringSubmatch(cmd); p != nil {
		f.pkg, f.Kind = normPath(p[1]), "zip"
	}
	if i := imageArgRe.FindStringSubmatch(cmd); i != nil {
		f.Image, f.Kind = imageRepo(i[1]), "docker"
	}
	if p := portArgRe.FindStringSubmatch(cmd); p != nil {
		f.Port, _ = strconv.Atoi(p[1])
	}
	if f.pkg != "" || f.Image != "" {
		s.emit(f, src, base)
	}
}

// hashtables handles PowerShell `@{ Path = 'apps/x'; Name = 'app' }`.
func (s *scanner) hashtables(text, src, base string) {
	for _, m := range hashRe.FindAllStringSubmatch(text, -1) {
		kv := map[string]string{}
		for _, part := range regexp.MustCompile(`[;\n]`).Split(m[1], -1) {
			if g := kvRe.FindStringSubmatch(part); g != nil {
				kv[strings.ToLower(g[1])] = psString(g[2])
			}
		}
		name := first(kv["name"], kv["appname"], kv["functionappname"], kv["webappname"])
		p := first(kv["path"], kv["apppath"], kv["folder"], kv["source"])
		if name != "" && p != "" {
			s.emit(Finding{AppName: name, pkg: normPath(p), Kind: "zip"}, src, base)
		}
	}
}

var quotedRe = regexp.MustCompile(`'([^']*)'|"([^"]*)"`)

// psString unquotes a PowerShell literal; `(Join-Path 'a' 'b')` yields the last quoted part.
func psString(v string) string {
	if strings.HasPrefix(v, "(Join-Path") {
		q := quotedRe.FindAllStringSubmatch(v, -1)
		if len(q) >= 2 {
			return strings.TrimSpace(first(q[len(q)-1][1], q[len(q)-1][2]))
		}
	}
	return strings.Trim(v, `'" `)
}

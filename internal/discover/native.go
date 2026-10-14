package discover

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// NativeRun proposes how to start a docker app without containers (`up --native`): the `run:` command line
// ({port} stands for the port to listen on) and the host_header it needs. It reads the final stage of the
// Dockerfile (CMD/ENTRYPOINT) and falls back to a dev server from package.json for static-site images.
// ok is false when nothing sensible can be derived; the caller then asks the user to write `run:`.
//
// ponytail: heuristics for the common shapes (uvicorn/gunicorn/flask, node, vite behind nginx), not a Dockerfile interpreter.
func NativeRun(appDir, dockerfile string) (run, hostHeader string, ok bool) {
	b, err := os.ReadFile(filepath.Join(appDir, filepath.FromSlash(dockerfile)))
	if err != nil {
		return "", "", false
	}
	base, cmd := finalStage(string(b))
	if cmd != "" && !staticServer(base) {
		if run = pythonCommand(normalizeCmd(cmd), appDir); run != "" {
			return run, "", true
		}
	}
	if dev := devServer(appDir); dev != "" && (cmd == "" || staticServer(base)) {
		return dev, devHostHeader(appDir), true
	}
	return "", "", false
}

var (
	fromRe    = regexp.MustCompile(`(?im)^\s*FROM\s+(\S+)`)
	portRe    = regexp.MustCompile(`\$\{PORT(?::-?\d+)?\}|\$PORT\b`)
	hostFlag  = regexp.MustCompile(`(--host[= ]|-H )\s*\S+`)
	bindAddr  = regexp.MustCompile(`\b0\.0\.0\.0:(\d+|\{port\})`)
	portFlag  = regexp.MustCompile(`(--port[= ]|-p )\s*\d+`)
	staticRe  = regexp.MustCompile(`(?i)(^|/)(nginx|httpd|caddy|apache)(:|$)`)
	pyScripts = map[string]bool{"uvicorn": true, "gunicorn": true, "hypercorn": true, "daphne": true, "flask": true, "fastapi": true}
)

func staticServer(baseImage string) bool { return staticRe.MatchString(baseImage) }

// finalStage returns the base image and the effective command (ENTRYPOINT + CMD) of the last build stage.
func finalStage(dockerfile string) (base, cmd string) {
	lines := logicalLines(dockerfile)
	var entry, cmdArgs []string
	var entrySet, cmdSet bool
	for _, l := range lines {
		word, rest, _ := strings.Cut(strings.TrimSpace(l), " ")
		switch strings.ToUpper(word) {
		case "FROM":
			if m := fromRe.FindStringSubmatch(l); m != nil {
				base = m[1]
			}
			entry, cmdArgs, entrySet, cmdSet = nil, nil, false, false
		case "ENTRYPOINT":
			entry, entrySet = parseExec(rest), true
		case "CMD":
			cmdArgs, cmdSet = parseExec(rest), true
		}
	}
	if !entrySet && !cmdSet {
		return base, ""
	}
	return base, strings.Join(append(entry, cmdArgs...), " ")
}

// logicalLines joins backslash continuations and drops comments.
func logicalLines(s string) []string {
	var out []string
	var cur string
	for _, raw := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(raw)
		if strings.HasPrefix(t, "#") && cur == "" {
			continue
		}
		if strings.HasSuffix(t, "\\") {
			cur += strings.TrimSuffix(t, "\\") + " "
			continue
		}
		out = append(out, cur+t)
		cur = ""
	}
	return out
}

// parseExec reads the argument of CMD/ENTRYPOINT: exec form (JSON array) or shell form. A `sh -c "..."` wrapper is unwrapped.
func parseExec(arg string) []string {
	arg = strings.TrimSpace(arg)
	var v []string
	if strings.HasPrefix(arg, "[") && json.Unmarshal([]byte(arg), &v) == nil {
		if len(v) >= 3 && (v[0] == "sh" || v[0] == "bash") && v[1] == "-c" {
			return []string{v[2]}
		}
		return v
	}
	return []string{arg}
}

// normalizeCmd turns a container command into one that listens on {port} of this machine.
func normalizeCmd(cmd string) string {
	cmd = portRe.ReplaceAllString(cmd, "{port}")
	cmd = hostFlag.ReplaceAllString(cmd, "${1}127.0.0.1")
	cmd = bindAddr.ReplaceAllString(cmd, "127.0.0.1:{port}")
	cmd = strings.ReplaceAll(cmd, "0.0.0.0", "127.0.0.1")
	cmd = portFlag.ReplaceAllString(cmd, "${1}{port}")
	return strings.Join(strings.Fields(cmd), " ")
}

// pythonCommand makes a python server command start from the app's own environment without console-script shims
// (`uvicorn` -> `python -m uvicorn`: freshly created .exe launchers are blocked on locked-down Windows machines).
// It returns "" for anything it does not recognize.
func pythonCommand(cmd, appDir string) string {
	first, rest, _ := strings.Cut(cmd, " ")
	switch {
	case pyScripts[first]:
		cmd = "python -m " + first + " " + rest
	case first == "python" || first == "python3":
		cmd = "python " + rest
	case first == "node" || first == "npm" || first == "npx":
		if strings.Contains(cmd, "{port}") {
			return cmd
		}
		return "" // node servers usually read $PORT, which azemu also sets, but we cannot tell
	default:
		return ""
	}
	cmd = strings.TrimSpace(cmd)
	if !strings.Contains(cmd, "{port}") { // servers whose command line never names the port
		switch {
		case strings.Contains(cmd, "gunicorn"):
			cmd += " -b 127.0.0.1:{port}"
		case strings.Contains(cmd, "uvicorn"), strings.Contains(cmd, "hypercorn"), strings.Contains(cmd, "daphne"), strings.Contains(cmd, "fastapi"):
			cmd += " --host 127.0.0.1 --port {port}"
		case strings.Contains(cmd, "flask"):
			cmd += " --host 127.0.0.1 --port {port}"
		default:
			return ""
		}
	}
	if _, err := os.Stat(filepath.Join(appDir, "uv.lock")); err == nil {
		return "uv run " + cmd
	}
	return cmd
}

type packageJSON struct {
	Scripts         map[string]string `json:"scripts"`
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
}

func readPackage(appDir string) *packageJSON {
	b, err := os.ReadFile(filepath.Join(appDir, "package.json"))
	if err != nil {
		return nil
	}
	var p packageJSON
	if json.Unmarshal(b, &p) != nil {
		return nil
	}
	return &p
}

// devServer is the `npm run dev` command of a front-end project, bound to {port}.
func devServer(appDir string) string {
	p := readPackage(appDir)
	if p == nil || p.Scripts["dev"] == "" {
		return ""
	}
	switch {
	case isVite(p) || strings.Contains(p.Scripts["dev"], "vite"):
		return "npm run dev -- --host 127.0.0.1 --port {port}"
	case strings.Contains(p.Scripts["dev"], "next"):
		return "npm run dev -- --hostname 127.0.0.1 --port {port}"
	}
	return ""
}

func isVite(p *packageJSON) bool {
	_, a := p.Dependencies["vite"]
	_, b := p.DevDependencies["vite"]
	return a || b
}

// devHostHeader: dev servers reject Host headers they do not know, and the ingress sends the Azure hostname.
func devHostHeader(appDir string) string {
	if devServer(appDir) != "" {
		return "local"
	}
	return ""
}

// FillNative sets run (and host_header) on every docker app that has none, where NativeRun can derive them.
// It returns the keys it could not fill, so the caller can tell the user to write `run:` by hand.
func (d *Proposal) FillNative(root string) (unresolved []string) {
	for k, a := range d.Config.Apps {
		if a.Kind != "docker" || a.Run != "" {
			continue
		}
		run, hh, ok := NativeRun(filepath.Join(root, filepath.FromSlash(a.Path)), a.Dockerfile)
		if !ok {
			unresolved = append(unresolved, k)
			continue
		}
		a.Run, a.HostHeader = run, hh
	}
	sort.Strings(unresolved)
	return unresolved
}

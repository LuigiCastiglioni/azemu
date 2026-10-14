package host

import (
	"fmt"
	"strconv"
	"strings"
)

// KillTree stops the process pid. On Windows that is the whole tree (taskkill /T /F: there is no polite way to
// stop a console tree). Elsewhere group=true signals the process group led by pid, which is how azemu starts
// every app, and force picks SIGKILL over SIGTERM. A process that is already gone is an error, as with kill(1).
func KillTree(pid int, group, force bool) error {
	if GOOS() == "windows" {
		return Run(Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)))
	}
	sig, target := "TERM", strconv.Itoa(pid)
	if force {
		sig = "KILL"
	}
	if group {
		target = "-" + target
	}
	return Run(Command("sh", "-c", fmt.Sprintf("kill -%s -- %s", sig, target)))
}

// ProcessCommandLines returns the command line of each pid that is still running; pids that are gone are absent.
// One query for all of them: on Windows it starts PowerShell, which is slow enough to avoid doing per process.
func ProcessCommandLines(pids []int) (map[int]string, error) {
	out := map[int]string{}
	if len(pids) == 0 {
		return out, nil
	}
	ids := make([]string, len(pids))
	for i, p := range pids {
		ids[i] = strconv.Itoa(p)
	}
	list := strings.Join(ids, ",")

	var b []byte
	if GOOS() == "windows" {
		// no double quotes: they would not survive the argument quoting of every host
		script := `Get-CimInstance Win32_Process | Where-Object { @(` + list + `) -contains $_.ProcessId } | ` +
			`ForEach-Object { $_.ProcessId.ToString() + [char]9 + [string]$_.CommandLine }`
		b, _ = Output(Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script))
	} else {
		b, _ = Output(Command("ps", "-o", "pid=,args=", "-p", list)) // exits 1 when none of them exist
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		var pidStr, rest string
		if GOOS() == "windows" {
			pidStr, rest, _ = strings.Cut(line, "\t")
		} else {
			pidStr, rest, _ = strings.Cut(line, " ")
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(pidStr)); err == nil {
			out[pid] = strings.TrimSpace(rest)
		}
	}
	return out, nil
}

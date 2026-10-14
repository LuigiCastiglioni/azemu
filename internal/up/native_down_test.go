package up

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luigi23997/azemu/internal/host"
)

func TestMatchFor(t *testing.T) {
	for _, tc := range []struct {
		l    *launch
		want string
	}{
		{&launch{shell: "uv run python -m uvicorn app:api --port 1"}, "uv"},
		{&launch{shell: `C:\tools\Node.exe server.js`}, "node"},
		{&launch{argv: []string{"func", "start", "--port", "7001"}}, "func"},
		{&launch{argv: []string{"npx", "--yes", "azurite"}}, "npx"},
	} {
		if got := matchFor(tc.l); got != tc.want {
			t.Errorf("%+v: %q, want %q", tc.l, got, tc.want)
		}
	}
}

func TestNativeStateLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".azemu", nativeStateFile)
	s, err := openNativeState(path)
	if err != nil {
		t.Fatal(err)
	}
	s.set("web", 4242, "uv")
	s.set("api", 4343, "func")
	s.remove("api", 1) // a restart already replaced that process: stays
	s.remove("web", 4242)
	got, err := readNativeProcs(path)
	if err != nil || got == nil {
		t.Fatalf("read: %v %v", got, err)
	}
	if got.Azemu.PID != host.Getpid() || got.Azemu.Match != "azemu" {
		t.Errorf("azemu entry = %+v", got.Azemu)
	}
	if _, ok := got.Procs["web"]; ok || got.Procs["api"] != (procEntry{4343, "func"}) {
		t.Errorf("procs = %+v", got.Procs)
	}
	s.clear()
	if p, _ := readNativeProcs(path); p != nil {
		t.Error("clear must remove the file")
	}
	(*nativeState)(nil).set("x", 1, "y") // a missing state must not panic
}

func TestSecondUpIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), nativeStateFile)
	// a live process that looks like azemu: this very test binary is not called azemu, so record what it is called
	cl, _ := host.ProcessCommandLines([]int{os.Getppid()})
	if cl[os.Getppid()] == "" {
		t.Skip("cannot read the parent's command line here")
	}
	other := nativeProcs{Azemu: procEntry{PID: os.Getppid(), Match: strings.ToLower(filepath.Base(strings.Fields(cl[os.Getppid()])[0]))}, Procs: map[string]procEntry{}}
	s := &nativeState{path: path, data: other}
	s.save()
	if _, err := openNativeState(path); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("want an already-running error, got %v", err)
	}
	// the same pid but a different program (pid reuse after a crash): the stale file is simply taken over
	s.data.Azemu.Match = "definitely-not-this-program"
	s.save()
	if _, err := openNativeState(path); err != nil {
		t.Fatalf("stale state must be replaced: %v", err)
	}
}

type fakeHost struct {
	mu     sync.Mutex
	live   map[int]string
	killed []int
	// killing a pid also ends these (a Windows process tree)
	tree map[int][]int
}

func (f *fakeHost) cmdlines(pids []int) (map[int]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[int]string{}
	for _, p := range pids {
		if cl, ok := f.live[p]; ok {
			out[p] = cl
		}
	}
	return out, nil
}

func (f *fakeHost) kill(pid int, group, force bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killed = append(f.killed, pid)
	delete(f.live, pid)
	for _, c := range f.tree[pid] {
		delete(f.live, c)
	}
	return nil
}

func TestStopRecorded(t *testing.T) {
	procs := &nativeProcs{
		Azemu: procEntry{10, "azemu"},
		Procs: map[string]procEntry{
			"azurite":  {11, "npx"},
			"api":      {12, "uv"},     // gone with azemu's tree
			"web":      {13, "npm"},    // pid reused by an unrelated program
			"worker":   {14, "func"},   // already dead before down
			"leftover": {15, "python"}, // survives azemu (not in its tree), must still be stopped
		},
	}
	f := &fakeHost{
		live: map[int]string{
			10: "node C:\\x\\azemu-wasm.js up --native",
			11: "node npx-cli.js --yes azurite", 12: "cmd.exe /c uv run uvicorn",
			13: "chrome.exe --type=renderer", 15: "python -m http.server",
		},
		tree: map[int][]int{10: {11, 12}},
	}
	lines := stopRecorded(procs, f.cmdlines, f.kill)
	joined := strings.Join(lines, "\n")

	if !slices.Equal(f.killed, []int{10, 15}) {
		t.Errorf("killed %v, want [10 15] (azemu first, then the survivor; never the reused pid)\n%s", f.killed, joined)
	}
	for _, want := range []string{
		"azemu: stopped", "azurite: stopped", "api: stopped", "leftover: stopped",
		"web: pid 13 is another program now (chrome.exe --type=renderer); left alone", "worker: stopped",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
}

func TestStopRecordedJudgesByResult(t *testing.T) {
	procs := &nativeProcs{Azemu: procEntry{10, "azemu"}, Procs: map[string]procEntry{"a": {11, "func"}}}
	f := &fakeHost{live: map[int]string{10: "node azemu-wasm.js up", 11: "func start"}}

	// kill reports an error but the process is gone (taskkill /T racing the tree's own exit): that is a success
	racy := func(pid int, group, force bool) error { f.kill(pid, group, force); return os.ErrProcessDone }
	if lines := stopRecorded(procs, f.cmdlines, racy); lines[0] != "azemu: stopped" {
		t.Errorf("died despite the error, want stopped: %q", lines)
	}

	// kill fails and the process is still there: that is an error
	f = &fakeHost{live: map[int]string{10: "node azemu-wasm.js up", 11: "func start"}}
	denied := func(pid int, group, force bool) error { return os.ErrPermission }
	if lines := stopRecorded(procs, f.cmdlines, denied); !strings.Contains(lines[0], "could not stop pid 10") {
		t.Errorf("want a could-not-stop line, got %q", lines)
	}
}

func TestStopRecordedNothingRunning(t *testing.T) {
	f := &fakeHost{live: map[int]string{}}
	lines := stopRecorded(&nativeProcs{Azemu: procEntry{10, "azemu"}, Procs: map[string]procEntry{"a": {11, "func"}}}, f.cmdlines, f.kill)
	if len(f.killed) != 0 || !slices.Equal(lines, []string{"azemu: not running", "a: not running"}) {
		t.Errorf("killed %v, lines %q", f.killed, lines)
	}
}

// The real thing: a process started the way `up --native` starts apps is found by its command line and stopped.
func TestStopRecordedRealProcess(t *testing.T) {
	argv, match := []string{"sleep", "30"}, "sleep"
	if host.GOOS() == "windows" {
		argv, match = []string{"ping", "-n", "30", "127.0.0.1"}, "ping"
	}
	var out bytes.Buffer
	p, err := startProc(t.TempDir(), os.Environ(), &prefixer{mu: &sync.Mutex{}, w: &out, name: "t"}, "", argv...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.stop)
	pid := p.cmd.Process.Pid
	time.Sleep(500 * time.Millisecond)

	procs := &nativeProcs{Azemu: procEntry{1 << 22, "azemu"}, Procs: map[string]procEntry{"app": {pid, match}}}
	lines := stopRecorded(procs, host.ProcessCommandLines, host.KillTree)
	if !strings.Contains(strings.Join(lines, "\n"), "app: stopped") {
		t.Fatalf("lines: %q", lines)
	}
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		t.Fatalf("process %d still running after down: %q", pid, lines)
	}
}

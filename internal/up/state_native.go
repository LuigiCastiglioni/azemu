package up

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/luigi23997/azemu/internal/host"
)

// nativeStateFile lists the processes of a running `up --native`, so that `down --native` can stop them from
// another terminal, or clean up after a crash or a closed window that left them behind.
const nativeStateFile = "native.json"

// procEntry is one recorded process. Match is a word that must appear in its live command line: pids are reused,
// and `down` must never kill an unrelated program that happens to have got a recorded number.
type procEntry struct {
	PID   int    `json:"pid"`
	Match string `json:"match"`
}

type nativeProcs struct {
	Azemu procEntry            `json:"azemu"`
	Procs map[string]procEntry `json:"procs"`
}

// nativeState keeps the file in step with the processes azemu starts and stops.
type nativeState struct {
	path string
	mu   sync.Mutex
	data nativeProcs
}

func readNativeProcs(path string) (*nativeProcs, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var d nativeProcs
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &d, nil
}

// openNativeState claims the project for this process. It fails when another `up --native` still runs there:
// two stacks would fight over the same ports and the same Azurite data.
func openNativeState(path string) (*nativeState, error) {
	if old, err := readNativeProcs(path); err == nil && old != nil && old.Azemu.PID > 0 && old.Azemu.PID != host.Getpid() {
		if cl, _ := host.ProcessCommandLines([]int{old.Azemu.PID}); strings.Contains(strings.ToLower(cl[old.Azemu.PID]), old.Azemu.Match) {
			return nil, fmt.Errorf("azemu --native is already running for this project (pid %d): stop it with Ctrl-C in its terminal, or run `azemu down --native`", old.Azemu.PID)
		}
	}
	s := &nativeState{path: path, data: nativeProcs{Azemu: procEntry{PID: host.Getpid(), Match: "azemu"}, Procs: map[string]procEntry{}}}
	return s, s.save()
}

func (s *nativeState) save() error {
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(s.path, b, 0o600)
}

func (s *nativeState) set(name string, pid int, match string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Procs[name] = procEntry{pid, match}
	s.save()
}

// remove forgets name, unless a restart has already put a different process there.
func (s *nativeState) remove(name string, pid int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.Procs[name].PID == pid {
		delete(s.data.Procs, name)
		s.save()
	}
}

func (s *nativeState) clear() {
	if s != nil {
		os.Remove(s.path)
	}
}

// matchFor is the word that identifies a launch in its process's command line.
func matchFor(l *launch) string {
	word := ""
	if l.shell != "" {
		word, _, _ = strings.Cut(strings.TrimSpace(l.shell), " ")
	} else if len(l.argv) > 0 {
		word = l.argv[0]
	}
	word = filepath.Base(strings.ReplaceAll(word, `\`, "/"))
	return strings.ToLower(strings.TrimSuffix(word, filepath.Ext(word)))
}

// DownNative is `azemu down --native`: stops what `up --native` started, wherever it runs. volumes also wipes
// the emulated storage, like `down --volumes` does for the containers' Azurite volume.
func DownNative(ctx context.Context, file string, volumes bool) error {
	cfg, _, err := load(file, "", nil)
	if err != nil {
		return err
	}
	path := cfg.StatePath(nativeStateFile)
	procs, err := readNativeProcs(path)
	if err != nil {
		return err
	}
	if procs == nil {
		fmt.Println("no native stack recorded (nothing started by `azemu up --native` here)")
	} else {
		for _, line := range stopRecorded(procs, host.ProcessCommandLines, host.KillTree) {
			fmt.Println(line)
		}
		os.Remove(path)
	}
	if volumes {
		if err := os.RemoveAll(cfg.StatePath("azurite")); err != nil {
			return fmt.Errorf("wiping emulated storage: %w (is Azurite still running?)", err)
		}
		fmt.Println("emulated storage wiped")
	}
	return nil
}

// stopRecorded stops azemu first (so nothing is restarted behind us), then whatever of its apps is left, and
// reports one line per process. cmdlines and kill are the host's, replaceable in tests.
func stopRecorded(p *nativeProcs, cmdlines func([]int) (map[int]string, error), kill func(pid int, group, force bool) error) []string {
	names := make([]string, 0, len(p.Procs))
	for n := range p.Procs {
		names = append(names, n)
	}
	slices.Sort(names)

	var lines []string
	// ours reports whether pid still runs the program we recorded. When it runs something else, line says so.
	ours := func(name string, e procEntry, live map[int]string) (running bool, line string) {
		cl, alive := live[e.PID]
		switch {
		case !alive:
			return false, ""
		case !strings.Contains(strings.ToLower(cl), e.Match):
			return false, fmt.Sprintf("%s: pid %d is another program now (%s); left alone", name, e.PID, abbreviate(cl))
		}
		return true, ""
	}
	stop := func(name string, e procEntry, group bool) string {
		if err := kill(e.PID, group, false); err != nil {
			// taskkill /T reports failure when part of the tree exits while it is being killed: judge by the result
			if live, _ := cmdlines([]int{e.PID}); live[e.PID] == "" {
				return name + ": stopped"
			}
			return fmt.Sprintf("%s: could not stop pid %d: %v", name, e.PID, err)
		}
		if host.GOOS() != "windows" { // SIGTERM may be ignored; give it a moment, then insist
			for i := 0; i < 10; i++ {
				time.Sleep(500 * time.Millisecond)
				if live, _ := cmdlines([]int{e.PID}); live[e.PID] == "" {
					return name + ": stopped"
				}
			}
			kill(e.PID, group, true)
		}
		return name + ": stopped"
	}

	live, _ := cmdlines([]int{p.Azemu.PID})
	azemuWasRunning := false
	if run, line := ours("azemu", p.Azemu, live); run {
		azemuWasRunning = true
		lines = append(lines, stop("azemu", p.Azemu, false))
	} else if line != "" {
		lines = append(lines, line)
	} else {
		lines = append(lines, "azemu: not running")
	}

	pids := make([]int, 0, len(names))
	for _, n := range names {
		pids = append(pids, p.Procs[n].PID)
	}
	live, _ = cmdlines(pids) // after azemu: on Windows its tree is gone with it
	for _, n := range names {
		e := p.Procs[n]
		run, line := ours(n, e, live)
		switch {
		case run:
			lines = append(lines, stop(n, e, true))
		case line != "":
			lines = append(lines, line)
		case azemuWasRunning:
			lines = append(lines, n+": stopped")
		default:
			lines = append(lines, n+": not running")
		}
	}
	return lines
}

func abbreviate(s string) string {
	if len(s) > 60 {
		return s[:57] + "..."
	}
	return s
}

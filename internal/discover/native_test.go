package discover

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/luigi23997/azemu/internal/config"
)

func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestNativeRun(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		run   string
		host  string
	}{
		{
			name: "uvicorn behind sh -c in a multi-stage build, uv project",
			files: map[string]string{"uv.lock": "", "Dockerfile": `FROM python:3.12-slim AS deps
CMD ["not", "this"]
FROM python:3.12-slim
ENV PORT=8000
CMD ["sh", "-c", "uvicorn src.main:app --host 0.0.0.0 --port ${PORT:-8000}"]
`},
			run: "uv run python -m uvicorn src.main:app --host 127.0.0.1 --port {port}",
		},
		{
			name: "gunicorn exec form with a bind address",
			files: map[string]string{"Dockerfile": `FROM python:3.12
CMD ["gunicorn", "-b", "0.0.0.0:8000", "app:app"]
`},
			run: "python -m gunicorn -b 127.0.0.1:{port} app:app",
		},
		{
			name:  "uvicorn shell form without a port gets one",
			files: map[string]string{"Dockerfile": "FROM python:3.12\nCMD uvicorn app:api\n"},
			run:   "python -m uvicorn app:api --host 127.0.0.1 --port {port}",
		},
		{
			name:  "line continuation and comment",
			files: map[string]string{"Dockerfile": "FROM python:3.12\n# start\nCMD uvicorn app:api \\\n  --host 0.0.0.0 --port 80\n"},
			run:   "python -m uvicorn app:api --host 127.0.0.1 --port {port}",
		},
		{
			name:  "node server naming its port",
			files: map[string]string{"Dockerfile": "FROM node:20\nCMD node server.js --port 8080\n"},
			run:   "node server.js --port {port}",
		},
		{
			name: "vite SPA served by nginx runs its dev server and needs the local Host header",
			files: map[string]string{
				"package.json": `{"scripts":{"dev":"vite"},"devDependencies":{"vite":"^5"}}`,
				"Dockerfile":   "FROM node:20 AS build\nRUN npm run build\nFROM nginx:alpine\nEXPOSE 80\n",
			},
			run: "npm run dev -- --host 127.0.0.1 --port {port}", host: "local",
		},
		{
			name:  "nginx with a CMD still prefers the dev server",
			files: map[string]string{"package.json": `{"scripts":{"dev":"vite --open"}}`, "Dockerfile": "FROM nginx\nCMD [\"nginx\", \"-g\", \"daemon off;\"]\n"},
			run:   "npm run dev -- --host 127.0.0.1 --port {port}", host: "local",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeFiles(t, tc.files)
			run, host, ok := NativeRun(dir, "Dockerfile")
			if !ok || run != tc.run || host != tc.host {
				t.Errorf("got (%q, %q, %v), want (%q, %q, true)", run, host, ok, tc.run, tc.host)
			}
		})
	}
}

func TestNativeRunGivesUp(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"no command":              {"Dockerfile": "FROM python:3.12\n"},
		"unknown program":         {"Dockerfile": "FROM x\nCMD [\"./server\"]\n"},
		"python script, no port":  {"Dockerfile": "FROM python:3.12\nCMD [\"python\", \"main.py\"]\n"},
		"node server, no port":    {"Dockerfile": "FROM node:20\nCMD [\"node\", \"server.js\"]\n"},
		"nginx, no dev script":    {"Dockerfile": "FROM nginx\n", "package.json": `{"scripts":{"build":"vite build"}}`},
		"missing dockerfile only": {},
	} {
		t.Run(name, func(t *testing.T) {
			if run, _, ok := NativeRun(writeFiles(t, files), "Dockerfile"); ok {
				t.Errorf("want no suggestion, got %q", run)
			}
		})
	}
}

func TestFillNative(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "apps", "api"), 0o755)
	os.MkdirAll(filepath.Join(root, "apps", "odd"), 0o755)
	os.WriteFile(filepath.Join(root, "apps", "api", "Dockerfile"), []byte("FROM python:3.12\nCMD uvicorn app:api --port 80\n"), 0o644)
	os.WriteFile(filepath.Join(root, "apps", "odd", "Dockerfile"), []byte("FROM scratch\nCMD [\"./x\"]\n"), 0o644)
	d := &Proposal{Config: &config.Config{Apps: map[string]*config.App{
		"api":  {Path: "apps/api", Kind: "docker", Dockerfile: "Dockerfile"},
		"odd":  {Path: "apps/odd", Kind: "docker", Dockerfile: "Dockerfile"},
		"keep": {Path: "apps/api", Kind: "docker", Dockerfile: "Dockerfile", Run: "mine"},
		"fn":   {Path: "apps/fn", Kind: "zip"},
	}}}
	un := d.FillNative(root)
	if got := d.Config.Apps["api"].Run; got != "python -m uvicorn app:api --port {port}" {
		t.Errorf("api run = %q", got)
	}
	if d.Config.Apps["keep"].Run != "mine" || d.Config.Apps["fn"].Run != "" {
		t.Errorf("must not touch existing run or zip apps: %+v %+v", d.Config.Apps["keep"], d.Config.Apps["fn"])
	}
	if len(un) != 1 || un[0] != "odd" {
		t.Errorf("unresolved = %v, want [odd]", un)
	}
}

func TestHostHeaderValidated(t *testing.T) {
	p := filepath.Join(t.TempDir(), config.FileName)
	os.WriteFile(p, []byte("env: dev\napps:\n  a:\n    path: a\n    kind: docker\n    host_header: nope\n"), 0o644)
	if _, err := config.Load(p); err == nil {
		t.Fatal("want an error for host_header: nope")
	}
	os.WriteFile(p, []byte("env: dev\napps:\n  a:\n    path: a\n    kind: docker\n    host_header: local\n"), 0o644)
	if _, err := config.Load(p); err != nil {
		t.Fatal(err)
	}
}

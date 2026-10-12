package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoginThenRefresh(t *testing.T) {
	sleep = func(context.Context, time.Duration) {}
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		switch {
		case strings.HasSuffix(r.URL.Path, "/devicecode"):
			if !strings.Contains(r.Form.Get("scope"), "Sites.Read.All") || !strings.Contains(r.Form.Get("scope"), "offline_access") {
				t.Errorf("scope: %q", r.Form.Get("scope"))
			}
			json.NewEncoder(w).Encode(map[string]any{"device_code": "dc", "interval": 1, "expires_in": 60, "message": "go to X, enter CODE"})
		case r.Form.Get("grant_type") == "refresh_token":
			if r.Form.Get("refresh_token") != "r2" {
				t.Errorf("refresh token: %q", r.Form.Get("refresh_token"))
			}
			json.NewEncoder(w).Encode(map[string]any{"access_token": "a3", "refresh_token": "r3", "expires_in": 3600})
		default:
			if polls++; polls == 1 {
				json.NewEncoder(w).Encode(map[string]any{"error": "authorization_pending"})
				return
			}
			idt := "x.eyJwcmVmZXJyZWRfdXNlcm5hbWUiOiJtZUBleGFtcGxlLmNvbSJ9.y" // {"preferred_username":"me@example.com"}
			json.NewEncoder(w).Encode(map[string]any{"access_token": "a1", "refresh_token": "r1", "id_token": idt, "expires_in": 3600})
		}
	}))
	defer srv.Close()

	var out bytes.Buffer
	tok, err := Login(context.Background(), srv.Client(), srv.URL, "organizations", "cid", []string{"Sites.Read.All"}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if polls != 2 || tok.Account != "me@example.com" || tok.Access != "a1" || !strings.Contains(out.String(), "CODE") {
		t.Fatalf("polls=%d tok=%+v out=%q", polls, tok, out.String())
	}

	path := filepath.Join(t.TempDir(), "g", TokenFile)
	src := &Source{Path: path, Client: srv.Client(), Authority: srv.URL}
	if _, err := src.Access(context.Background()); err == nil || !strings.Contains(err.Error(), "azemu graph login") {
		t.Fatalf("want login hint, got %v", err)
	}
	tok.Refresh, tok.Expires = "r2", time.Now().Add(-time.Hour) // expired: must refresh
	if err := Save(path, tok); err != nil {
		t.Fatal(err)
	}
	if a, err := src.Access(context.Background()); err != nil || a != "a3" {
		t.Fatalf("refresh: %q %v", a, err)
	}
	if saved, _ := Load(path); saved.Refresh != "r3" {
		t.Fatalf("rotated refresh token not saved: %+v", saved)
	}
	if a, _ := src.Access(context.Background()); a != "a3" { // valid now: no second refresh (server would fail the r2 check)
		t.Fatalf("cached: %q", a)
	}
}

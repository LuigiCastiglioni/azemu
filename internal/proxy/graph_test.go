package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luigi23997/azemu/internal/graph"
)

func testGate(t *testing.T, readOnly bool) (g *graphGate, entraHits, graphAuth *[]string) {
	t.Helper()
	entraHits, graphAuth = new([]string), new([]string)
	entra := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { *entraHits = append(*entraHits, r.URL.Path) }))
	gr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*graphAuth = append(*graphAuth, r.Header.Get("Authorization"))
	}))
	t.Cleanup(entra.Close)
	t.Cleanup(gr.Close)
	file := filepath.Join(t.TempDir(), graph.TokenFile)
	if err := graph.Save(file, &graph.Token{Access: "real", Expires: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	id, _ := newIdentity("h")
	eu, _ := url.Parse(entra.URL)
	gu, _ := url.Parse(gr.URL)
	return &graphGate{id: id, readOnly: readOnly, src: &graph.Source{Path: file},
		entra: newProxy(eu, "", http.DefaultTransport, nil), graph: newProxy(gu, "", http.DefaultTransport, nil)}, entraHits, graphAuth
}

func tokenRequest(g *graphGate, form url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/tenant-id/oauth2/v2.0/token", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	g.loginHandler().ServeHTTP(w, r)
	return w
}

func TestGraphTokenShimAndSwap(t *testing.T) {
	g, entraHits, graphAuth := testGate(t, true)

	// Graph client-credentials: fake token, nothing reaches Entra.
	w := tokenRequest(g, url.Values{"grant_type": {"client_credentials"}, "client_id": {"x"}, "client_secret": {"y"}, "scope": {"https://graph.microsoft.com/.default"}})
	if len(*entraHits) != 0 || !strings.Contains(w.Body.String(), `"access_token"`) {
		t.Fatalf("shim: entra=%v body=%s", *entraHits, w.Body)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := jsonDecode(w.Body.String(), &tok); err != nil || !isFakeGraphToken("Bearer "+tok.AccessToken) {
		t.Fatalf("not a marked token: %s", w.Body)
	}
	// Any other scope or grant goes to the real Entra.
	tokenRequest(g, url.Values{"grant_type": {"client_credentials"}, "scope": {"https://cognitiveservices.azure.com/.default"}})
	tokenRequest(g, url.Values{"grant_type": {"refresh_token"}, "scope": {"https://graph.microsoft.com/.default"}})
	if len(*entraHits) != 2 {
		t.Fatalf("passthrough: %v", *entraHits)
	}

	do := func(method, path, auth, body string) int {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", auth)
		w := httptest.NewRecorder()
		g.graphHandler().ServeHTTP(w, r)
		return w.Code
	}
	fake := "Bearer " + tok.AccessToken
	if c := do("GET", "/v1.0/sites/root", fake, ""); c != 200 || (*graphAuth)[0] != "Bearer real" {
		t.Fatalf("swap: %d %v", c, *graphAuth)
	}
	if c := do("GET", "/v1.0/sites/root", "Bearer someone-elses", ""); c != 200 || (*graphAuth)[1] != "Bearer someone-elses" {
		t.Fatalf("foreign token must pass through: %d %v", c, *graphAuth)
	}
	for _, tc := range []struct {
		method, path, body string
		want               int
	}{
		{"POST", "/v1.0/search/query", `{}`, 200},
		{"POST", "/v1.0/$batch", `{"requests":[{"id":"1","method":"GET","url":"/me"}]}`, 200},
		{"POST", "/v1.0/$batch", `{"requests":[{"id":"1","method":"DELETE","url":"/drive/items/1"}]}`, 403},
		{"PATCH", "/v1.0/drive/items/1", `{}`, 403},
		{"PUT", "/v1.0/drive/root:/a.txt:/content", `x`, 403},
	} {
		if c := do(tc.method, tc.path, fake, tc.body); c != tc.want {
			t.Errorf("%s %s: got %d want %d", tc.method, tc.path, c, tc.want)
		}
	}
	g.readOnly = false
	if c := do("PATCH", "/v1.0/drive/items/1", fake, `{}`); c != 200 {
		t.Errorf("writable: %d", c)
	}
}

func TestGraphNoLogin(t *testing.T) {
	g, _, _ := testGate(t, true)
	g.src = &graph.Source{Path: filepath.Join(t.TempDir(), "missing.json")}
	tok, _ := g.id.mint("https://graph.microsoft.com", map[string]any{graphMarker: "graph"})
	r := httptest.NewRequest("GET", "/v1.0/me", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	g.graphHandler().ServeHTTP(w, r)
	if w.Code != 401 || !strings.Contains(w.Body.String(), "azemu graph login") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestUpstreamDNS(t *testing.T) {
	conf := "nameserver 127.0.0.11\noptions ndots:0\n\n# ExtServers: [host(127.0.0.53) host(192.168.65.7) 10.0.0.2]\n"
	got := strings.Join(upstreamDNS(conf), ",")
	if got != "192.168.65.7,10.0.0.2,1.1.1.1,8.8.8.8" {
		t.Fatal(got)
	}
}

func jsonDecode(s string, v any) error { return json.NewDecoder(strings.NewReader(s)).Decode(v) }

package proxy

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luigi23997/azemu/internal/contract"
)

func TestCALeafVerifies(t *testing.T) {
	dir := t.TempDir()
	first, err := EnsureCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second, _ := EnsureCA(dir); string(first) != string(second) {
		t.Fatal("EnsureCA must reuse the existing CA")
	}
	c, _ := loadCA(dir)
	pair, err := c.getCertificate(&tls.ClientHelloInfo{ServerName: "acct.blob.core.windows.net"})
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(pair.Certificate[0])
	pool := x509.NewCertPool()
	pool.AddCert(c.cert)
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "acct.blob.core.windows.net", Roots: pool}); err != nil {
		t.Fatal(err)
	}
}

func TestIdentityToken(t *testing.T) {
	id, _ := newIdentity("secret")
	h := id.handler()
	get := func(hdr, target string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", target, nil)
		r.Header.Set("X-IDENTITY-HEADER", hdr)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := get("wrong", contract.IdentityPath+"?resource=x"); w.Code != 403 {
		t.Fatalf("want 403, got %d", w.Code)
	}
	w := get("secret", contract.IdentityPath+"?api-version=2019-08-01&scope=https://storage.azure.com/.default")
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["resource"] != "https://storage.azure.com" {
		t.Fatalf("resource: %v", resp)
	}
	var claims map[string]any
	payload, _ := base64.RawURLEncoding.DecodeString(strings.Split(resp["access_token"], ".")[1])
	json.Unmarshal(payload, &claims)
	if claims["aud"] != resp["resource"] || claims["iss"] != "https://sts.windows.net/"+contract.TenantID+"/" {
		t.Fatalf("claims: %v", claims)
	}
}

func TestIngressEasyAuth(t *testing.T) {
	var got http.Header
	var host string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got, host = r.Header, r.Host }))
	defer up.Close()
	h, _ := ingressHandler(contract.Ingress{Upstream: up.URL, Host: "app.azurewebsites.net", EasyAuth: true},
		contract.Principal{Name: "dev@example.com", ID: "oid-1"}, http.DefaultTransport)
	r := httptest.NewRequest("GET", "/x", nil)
	r.Header.Set("X-MS-CLIENT-PRINCIPAL-NAME", "spoofed@evil.com")
	h.ServeHTTP(httptest.NewRecorder(), r)
	if host != "app.azurewebsites.net" || got.Get("X-MS-CLIENT-PRINCIPAL-NAME") != "dev@example.com" || got.Get("X-MS-CLIENT-PRINCIPAL-IDP") != "aad" {
		t.Fatalf("host=%q headers=%v", host, got)
	}
	var p struct{ Claims []map[string]string }
	b, _ := base64.StdEncoding.DecodeString(got.Get("X-MS-CLIENT-PRINCIPAL"))
	if json.Unmarshal(b, &p) != nil || len(p.Claims) != 2 {
		t.Fatalf("principal: %s", b)
	}
}

func TestNoSNICertCoversRouteHosts(t *testing.T) {
	c, _ := loadCA(t.TempDir())
	c.noSNI = []string{"app.azurewebsites.net", "acct.blob.core.windows.net"}
	pair, err := c.getCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(pair.Certificate[0])
	pool := x509.NewCertPool()
	pool.AddCert(c.cert)
	for _, h := range c.noSNI {
		if _, err := leaf.Verify(x509.VerifyOptions{DNSName: h, Roots: pool}); err != nil {
			t.Fatalf("%s: %v", h, err)
		}
	}
}

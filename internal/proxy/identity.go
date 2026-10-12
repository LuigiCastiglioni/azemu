package proxy

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/luigi23997/azemu/internal/contract"
)

type identity struct {
	header string
	key    *rsa.PrivateKey
	oid    string // stable per proxy run
	now    func() time.Time
}

func newIdentity(header string) (*identity, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	return &identity{header: header, key: key, oid: uuid(), now: time.Now}, nil
}

func uuid() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// token mints an RS256 JWT for aud. ponytail: signature is never verified by Azurite (--oauth basic); the key is just for a well-formed JWT.
func (id *identity) token(aud string) (string, error) { return id.mint(aud, nil) }

// mint is token plus extra claims.
func (id *identity) mint(aud string, extra map[string]any) (string, error) {
	now := id.now()
	hdr, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": "azemu"})
	c := map[string]any{
		"aud": aud, "iss": "https://sts.windows.net/" + contract.TenantID + "/",
		"tid": contract.TenantID, "oid": id.oid, "sub": id.oid, "appid": id.oid, "idtyp": "app",
		"iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	}
	for k, v := range extra {
		c[k] = v
	}
	claims, _ := json.Marshal(c)
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString(hdr) + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, id.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

func (id *identity) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+contract.ProxyHealthPath, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("GET "+contract.IdentityPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-IDENTITY-HEADER") != id.header {
			http.Error(w, "invalid X-IDENTITY-HEADER", http.StatusForbidden)
			return
		}
		q := r.URL.Query()
		res := q.Get("resource")
		if res == "" {
			res = q.Get("scope")
		}
		res = strings.TrimSuffix(res, "/.default")
		if res == "" {
			http.Error(w, "missing resource", http.StatusBadRequest)
			return
		}
		tok, err := id.token(res)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"access_token": tok, "token_type": "Bearer", "resource": res,
			"expires_on": strconv.FormatInt(id.now().Add(time.Hour).Unix(), 10), "expires_in": "3600",
		})
	})
	return mux
}

// Package contract holds the names and paths every azemu component agrees on.
// Change a value here and every package follows; never hard-code these elsewhere.
package contract

import (
	"os"
	"path/filepath"
)

const (
	// Directory (relative to the azemu.yaml dir) holding all generated state. Gitignored.
	StateDir = ".azemu"

	// Docker compose service names.
	ProxyService   = "azemu-proxy"
	AzuriteService = "azurite"

	// Inside every container: shared volume written by the proxy at startup.
	CertsDir      = "/azemu/certs"
	CACertFile    = CertsDir + "/ca.pem"     // local root CA only (NODE_EXTRA_CA_CERTS)
	CABundleFile  = CertsDir + "/bundle.pem" // system roots + local CA (REQUESTS_CA_BUNDLE, SSL_CERT_FILE, CURL_CA_BUNDLE)
	AzuriteCert   = CertsDir + "/azurite.pem"
	AzuriteKeyPEM = CertsDir + "/azurite-key.pem"
	CertsVolume   = "azemu-certs"

	// Proxy container paths.
	ProxyConfigPath = "/azemu/proxy.json" // bind-mounted from <StateDir>/proxy.json
	ProxyCADir      = "/azemu/ca"         // bind-mounted from ~/.azemu/ca (persistent root CA)

	// Proxy ports (inside the network).
	ProxyTLSPort    = 443
	IdentityPort    = 8081
	IdentityPath    = "/msi/token"
	IngressBasePort = 7001       // first host port for app ingress
	ProxyHealthPath = "/healthz" // served on IdentityPort

	// Azurite ports.
	AzuriteBlobPort  = 10000
	AzuriteQueuePort = 10001
	AzuriteTablePort = 10002

	// Every emulated storage account uses the well-known Azurite key.
	AzuriteKey = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="

	// Fake tenant used in minted tokens.
	TenantID = "00000000-0000-0000-0000-00000000a2e0"
)

// IdentityEndpoint is the IDENTITY_ENDPOINT value injected into app containers.
func IdentityEndpoint() string {
	return "http://" + ProxyService + ":8081" + IdentityPath
}

// HostCADir is the persistent root CA directory on the host (ca.pem, ca-key.pem),
// bind-mounted into the proxy at ProxyCADir. Shared by all projects.
func HostCADir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".azemu", "ca")
}

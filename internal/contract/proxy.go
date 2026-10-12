package contract

// ProxyConfig is written by the CLI to <StateDir>/proxy.json and read by `azemu proxy`.
type ProxyConfig struct {
	IdentityHeader string            `json:"identity_header"` // expected X-IDENTITY-HEADER value
	Routes         map[string]string `json:"routes"`          // TLS SNI host -> upstream URL, e.g. "eund-func-orders.azurewebsites.net": "http://func-orders:80", "acct.blob.core.windows.net": "https://azurite:10000"
	Ingress        []Ingress         `json:"ingress"`
	Principal      Principal         `json:"principal"`       // Easy Auth identity injected on ingress
	Graph          *Graph            `json:"graph,omitempty"` // set when apps reach Microsoft Graph with an app registration
}

// Ingress exposes one app on a host port, published by compose on the proxy service.
type Ingress struct {
	Port     int    `json:"port"`
	Upstream string `json:"upstream"` // e.g. "http://func-orders:80"
	Host     string `json:"host"`     // Azure hostname, sent as Host header upstream
	EasyAuth bool   `json:"easy_auth"`
}

type Principal struct {
	Name string `json:"name"` // email / UPN
	ID   string `json:"id"`   // object id
}

// Graph makes the proxy pose as the app registration: client-credentials token requests for Graph get a fake token,
// and the fake token is swapped for the signed-in user's delegated token on graph.microsoft.com.
type Graph struct {
	ReadOnly bool `json:"read_only"` // reject anything but reads (GET, read-only $batch, search/query)
}

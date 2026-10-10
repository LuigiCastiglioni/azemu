//go:build integration

package proxy

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luigi23997/azemu/internal/contract"
)

// Spike a: python SDKs -> proxy (TLS, local CA, fake MSI) -> Azurite with production-style hosts.
// Run: go test -tags=integration ./internal/proxy/ -run Integration -v -timeout 20m

const spikeScript = `
import os, sys, uuid, traceback, requests
from azure.identity import DefaultAzureCredential
from azure.storage.blob import BlobServiceClient
from azure.data.tables import TableServiceClient
from azure.storage.queue import QueueServiceClient

KEY = os.environ["AZURITE_KEY"]
CONN = f"DefaultEndpointsProtocol=https;AccountName=testacct;AccountKey={KEY};EndpointSuffix=core.windows.net"
failed = 0

def step(mode, op, fn):
    global failed
    try:
        fn()
        print(f"RESULT {mode} {op} PASS", flush=True)
    except Exception as e:
        failed += 1
        traceback.print_exc()
        print(f"RESULT {mode} {op} FAIL {type(e).__name__}: {str(e)[:300]!r}", flush=True)

def run(mode, blob, table, queue):
    sfx = uuid.uuid4().hex[:8]
    def b():
        c = blob.create_container("c" + sfx)
        c.upload_blob("hello.txt", b"hello azemu")
        assert c.download_blob("hello.txt").readall() == b"hello azemu"
    def t():
        tc = table.create_table("t" + sfx)
        tc.upsert_entity({"PartitionKey": "p", "RowKey": "r", "v": "x"})
        assert tc.get_entity("p", "r")["v"] == "x"
    def q():
        qc = queue.create_queue("q" + sfx)
        qc.send_message("hi")
        assert [m.content for m in qc.receive_messages()] == ["hi"]
    step(mode, "blob", b); step(mode, "table", t); step(mode, "queue", q)

cred = DefaultAzureCredential()
run("msi", BlobServiceClient("https://testacct.blob.core.windows.net", credential=cred),
    TableServiceClient(endpoint="https://testacct.table.core.windows.net", credential=cred),
    QueueServiceClient("https://testacct.queue.core.windows.net", credential=cred))
run("sharedkey", BlobServiceClient.from_connection_string(CONN),
    TableServiceClient.from_connection_string(CONN), QueueServiceClient.from_connection_string(CONN))
step("external", "pypi", lambda: requests.get("https://pypi.org/simple/", timeout=30).raise_for_status())
sys.exit(1 if failed else 0)
`

func dockerRun(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func eventually(t *testing.T, what string, d time.Duration, f func() bool) time.Duration {
	t.Helper()
	start := time.Now()
	for time.Since(start) < d {
		if f() {
			return time.Since(start)
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
	return 0
}

func TestIntegrationAzurite(t *testing.T) {
	id := fmt.Sprintf("azemu-test-%06d", rand.Intn(1000000))
	net, vol, proxyName, azName := id, id+"-certs", id+"-proxy", id+"-azurite"
	t.Cleanup(func() {
		exec.Command("docker", "rm", "-f", proxyName, azName).Run()
		exec.Command("docker", "network", "rm", net).Run()
		exec.Command("docker", "volume", "rm", vol).Run()
	})

	t0 := time.Now()
	dockerRun(t, "build", "-t", "azemu/proxy:dev", "../..")
	t.Logf("proxy image build: %s", time.Since(t0).Round(time.Second))

	dir := t.TempDir()
	caDir := filepath.Join(dir, "ca")
	os.MkdirAll(caDir, 0o755)
	routes := map[string]string{}
	var aliases []string
	for svc, port := range map[string]int{"blob": contract.AzuriteBlobPort, "queue": contract.AzuriteQueuePort, "table": contract.AzuriteTablePort} {
		h := fmt.Sprintf("testacct.%s.core.windows.net", svc)
		routes[h] = fmt.Sprintf("https://azurite:%d", port)
		aliases = append(aliases, "--network-alias", h)
	}
	cfgPath := filepath.Join(dir, "proxy.json")
	cfg, _ := json.Marshal(contract.ProxyConfig{IdentityHeader: "spike-secret", Routes: routes})
	os.WriteFile(cfgPath, cfg, 0o644)

	dockerRun(t, "network", "create", net)
	dockerRun(t, "volume", "create", vol)
	dockerRun(t, append([]string{"run", "-d", "--name", proxyName, "--network", net, "--network-alias", contract.ProxyService,
		"-v", vol + ":" + contract.CertsDir, "-v", caDir + ":" + contract.ProxyCADir, "-v", cfgPath + ":" + contract.ProxyConfigPath + ":ro"},
		append(aliases, "azemu/proxy:dev")...)...)
	eventually(t, "proxy healthy", 30*time.Second, func() bool {
		return exec.Command("docker", "exec", proxyName, "wget", "-qO-", "http://127.0.0.1:8081/healthz").Run() == nil
	})

	t1 := time.Now()
	dockerRun(t, "run", "-d", "--name", azName, "--network", net, "--network-alias", "azurite",
		"-v", vol+":"+contract.CertsDir+":ro", "-e", "AZURITE_ACCOUNTS=testacct:"+contract.AzuriteKey,
		"mcr.microsoft.com/azure-storage/azurite", "azurite", "--blobHost", "0.0.0.0", "--queueHost", "0.0.0.0", "--tableHost", "0.0.0.0",
		"--oauth", "basic", "--cert", contract.AzuriteCert, "--key", contract.AzuriteKeyPEM, "--skipApiVersionCheck", "--loose", "--debug", "/tmp/az.log")
	eventually(t, "azurite ready", 3*time.Minute, func() bool {
		return strings.Contains(dockerRun(t, "logs", azName), "Table service is successfully listening")
	})
	t.Logf("azurite ready (incl. image pull): %s", time.Since(t1).Round(time.Second))

	scriptPath := filepath.Join(dir, "spike.py")
	os.WriteFile(scriptPath, []byte(spikeScript), 0o644)
	t2 := time.Now()
	out, err := exec.Command("docker", "run", "--rm", "--network", net, "-v", vol+":"+contract.CertsDir+":ro", "-v", scriptPath+":/spike.py:ro",
		"-e", "IDENTITY_ENDPOINT="+contract.IdentityEndpoint(), "-e", "IDENTITY_HEADER=spike-secret",
		"-e", "REQUESTS_CA_BUNDLE="+contract.CABundleFile, "-e", "SSL_CERT_FILE="+contract.CABundleFile, "-e", "CURL_CA_BUNDLE="+contract.CABundleFile,
		"-e", "AZURITE_KEY="+contract.AzuriteKey, "python:3.12-slim", "sh", "-c",
		"pip install -q --disable-pip-version-check azure-storage-blob azure-data-tables azure-storage-queue azure-identity requests && python /spike.py").CombinedOutput()
	t.Logf("python spike (incl. pip install): %s", time.Since(t2).Round(time.Second))
	t.Logf("python output:\n%s", out)
	for _, l := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(l, "RESULT ") && strings.Contains(l, " FAIL") {
			t.Error(l)
		}
	}
	if err != nil {
		t.Errorf("spike script: %v", err)
		t.Logf("proxy logs:\n%s", dockerRun(t, "logs", proxyName))
		t.Logf("azurite logs:\n%s", dockerRun(t, "logs", azName))
		t.Logf("azurite debug log:\n%s", dockerRun(t, "exec", azName, "tail", "-n", "60", "/tmp/az.log"))
	}
}

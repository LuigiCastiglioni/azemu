//go:build integration

package stack

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/luigi23997/azemu/internal/contract"
	"github.com/luigi23997/azemu/internal/infra"
)

// Needs Docker. Runs a TLS Azurite (like the real stack) on host ports 20000-20002.
func TestCreateObjectsAzurite(t *testing.T) {
	dir := t.TempDir()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "azemu-test"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	ca, _ := x509.ParseCertificate(caDER)
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafDER, _ := x509.CreateCertificate(rand.Reader, &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "azurite"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}, ca, &leafKey.PublicKey, caKey)
	keyDER, _ := x509.MarshalECPrivateKey(leafKey)
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	os.WriteFile(filepath.Join(dir, "cert.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), 0o644)
	os.WriteFile(filepath.Join(dir, "key.pem"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o644)

	name := "azemu-test-azurite-it"
	exec.Command("docker", "rm", "-f", name).Run()
	out, err := exec.Command("docker", "run", "-d", "--name", name, "-v", dir+":/c:ro",
		"-p", "127.0.0.1:20000:10000", "-p", "127.0.0.1:20001:10001", "-p", "127.0.0.1:20002:10002",
		"-e", "AZURITE_ACCOUNTS=acct1:"+contract.AzuriteKey, "mcr.microsoft.com/azure-storage/azurite",
		"azurite", "--blobHost", "0.0.0.0", "--queueHost", "0.0.0.0", "--tableHost", "0.0.0.0", "--oauth", "basic",
		"--cert", "/c/cert.pem", "--key", "/c/key.pem", "--skipApiVersionCheck", "--loose").CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", name).Run() })
	for i := 0; i < 30; i++ { // wait for the listener
		if c, err := net.Dial("tcp", "127.0.0.1:20002"); err == nil {
			c.Close()
			break
		}
		time.Sleep(time.Second)
	}

	m := &infra.Model{StorageAccounts: []infra.StorageAccount{{Name: "acct1", Containers: []string{"cont1", "cont2"}, Tables: []string{"tab1"}, Queues: []string{"queue1"}}}}
	for i := 0; i < 2; i++ { // second pass: everything already exists
		if err := createObjects(context.Background(), m, caPEM, "127.0.0.1", 20000, 20001, 20002); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
}

//go:build integration

package pull

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/data/aztables"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"

	"github.com/luigi23997/azemu/internal/contract"
)

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// azurite starts a container publishing blob/table on the given host ports and waits for it.
func azurite(t *testing.T, blobPort, tablePort int, extra ...string) {
	name := fmt.Sprintf("azemu-pull-it-%d", blobPort)
	args := append([]string{"run", "-d", "--name", name, "-p", fmt.Sprintf("127.0.0.1:%d:10000", blobPort), "-p", fmt.Sprintf("127.0.0.1:%d:10002", tablePort)}, extra...)
	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", name).Run() })
	for i := 0; i < 60; i++ {
		c1, e1 := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", blobPort))
		c2, e2 := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", tablePort))
		if e1 == nil && e2 == nil {
			c1.Close()
			c2.Close()
			time.Sleep(time.Second)
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatal("azurite not listening")
}

// tlsCert returns the CA PEM and a directory holding a 127.0.0.1 leaf cert/key signed by it.
func tlsCert(t *testing.T) ([]byte, string) {
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
	os.WriteFile(filepath.Join(dir, "cert.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), 0o644)
	os.WriteFile(filepath.Join(dir, "key.pem"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o644)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), dir
}

func sp(s string) *string { return &s }

func or(a, b *string) *string {
	if a == nil {
		return b
	}
	return a
}

func read(t *testing.T, c *azblob.Client, cont, name string) ([]byte, blob.GetPropertiesResponse) {
	ctx := context.Background()
	r, err := c.DownloadStream(ctx, cont, name, nil)
	if err != nil {
		t.Fatalf("download %s/%s: %v", cont, name, err)
	}
	b, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	p, err := c.ServiceClient().NewContainerClient(cont).NewBlobClient(name).GetProperties(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	return b, p
}

// entities lists a table as stripped full-metadata JSON, keyed by pk/rk.
func entities(t *testing.T, c *aztables.ServiceClient, table string) map[string]string {
	out := map[string]string{}
	pg := c.NewClient(table).NewListEntitiesPager(&aztables.ListEntitiesOptions{Format: to(aztables.MetadataFormatFull)})
	for pg.More() {
		p, err := pg.NextPage(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, raw := range p.Entities {
			b, key, err := stripEntity(raw)
			if err != nil {
				t.Fatal(err)
			}
			out[key] = string(b)
		}
	}
	return out
}

// Needs Docker: two Azurite containers, one playing real Azure (plain http, via the connection string
// override) and one the TLS local emulator account "acct1".
func TestPullAzurite(t *testing.T) {
	ctx := context.Background()
	sb, st, lb, lt := freePort(t), freePort(t), freePort(t), freePort(t)
	azurite(t, sb, st, "mcr.microsoft.com/azure-storage/azurite", "azurite", "--blobHost", "0.0.0.0", "--tableHost", "0.0.0.0", "--skipApiVersionCheck", "--loose")
	caPEM, dir := tlsCert(t)
	azurite(t, lb, lt, "-v", dir+":/c:ro", "-e", "AZURITE_ACCOUNTS=acct1:"+contract.AzuriteKey, "mcr.microsoft.com/azure-storage/azurite",
		"azurite", "--blobHost", "0.0.0.0", "--tableHost", "0.0.0.0", "--oauth", "basic", "--cert", "/c/cert.pem", "--key", "/c/key.pem", "--skipApiVersionCheck", "--loose")

	conn := fmt.Sprintf("DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;AccountKey=%s;BlobEndpoint=http://127.0.0.1:%d/devstoreaccount1;TableEndpoint=http://127.0.0.1:%d/devstoreaccount1;", contract.AzuriteKey, sb, st)
	src, err := SourceClients(conn)
	if err != nil {
		t.Fatal(err)
	}
	dst, err := LocalClients("acct1", caPEM, "127.0.0.1", lb, lt)
	if err != nil {
		t.Fatal(err)
	}

	// Seed the source.
	big := make([]byte, 10<<20)
	rand.Read(big)
	blobs := map[string][]byte{"a/small.txt": []byte("hello"), "a/empty": {}, "a/big.bin": big, "a/sub/x.json": []byte(`{"k":1}`), "b/other.txt": []byte("other")}
	if _, err := src.Blob.CreateContainer(ctx, "docs", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Blob.CreateContainer(ctx, "unselected", nil); err != nil {
		t.Fatal(err)
	}
	for n, b := range blobs {
		_, err := src.Blob.UploadBuffer(ctx, "docs", n, b, &azblob.UploadBufferOptions{
			Metadata: map[string]*string{"owner": sp("team-" + n)},
			HTTPHeaders: &blob.HTTPHeaders{BlobContentType: sp("text/x-test"), BlobContentEncoding: sp("identity"), BlobCacheControl: sp("max-age=60"),
				BlobContentDisposition: sp(`attachment; filename="f"`)},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	src.Blob.UploadBuffer(ctx, "unselected", "x", []byte("x"), nil)
	if _, err := src.Table.CreateTable(ctx, "Mixed", nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 25; i++ {
		e := aztables.EDMEntity{Entity: aztables.Entity{PartitionKey: fmt.Sprint("p", i%3), RowKey: fmt.Sprint("r", i)}, Properties: map[string]any{
			"S": "text é", "I32": int32(i), "I64": aztables.EDMInt64(9007199254740993 + int64(i)), "D": 1.5, "Whole": 2.0, "Bool": i%2 == 0,
			"When": aztables.EDMDateTime(time.Date(2024, 5, 6, 7, 8, 9, 123000000, time.UTC)), "Bin": aztables.EDMBinary([]byte{0, 1, 2, 255}),
			"G": aztables.EDMGUID("0f8fad5b-d9cb-469f-a165-70867728950e"),
		}}
		b, _ := json.Marshal(e)
		if _, err := src.Table.NewClient("Mixed").AddEntity(ctx, b, nil); err != nil {
			t.Fatal(err)
		}
	}

	var out bytes.Buffer
	o := Options{Containers: []string{"docs"}, Prefix: "a/", Tables: []string{"Mixed"}, Concurrency: 4}
	stats, err := Run(ctx, src, dst, o, &out)
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	t.Log("\n" + out.String())
	if s := stats[0]; s.Items != 4 || s.Skipped != 0 || s.Bytes != int64(len(big)+5+0+7) {
		t.Errorf("container stat %+v", s)
	}
	if s := stats[1]; s.Items != 25 || s.Failed != 0 {
		t.Errorf("table stat %+v", s)
	}

	// Exact copies; the prefix keeps b/ and the unselected container out.
	for n, want := range blobs {
		if n == "b/other.txt" {
			if _, err := dst.Blob.DownloadStream(ctx, "docs", n, nil); err == nil {
				t.Errorf("%s copied despite --prefix", n)
			}
			continue
		}
		got, p := read(t, dst.Blob, "docs", n)
		_, sp0 := read(t, src.Blob, "docs", n)
		if !bytes.Equal(got, want) {
			t.Errorf("%s: content differs", n)
		}
		if v := func(s *string) string { return *or(s, sp("<nil>")) }; v(p.ContentType) != "text/x-test" || v(p.ContentEncoding) != "identity" ||
			v(p.CacheControl) != "max-age=60" || v(p.ContentDisposition) != `attachment; filename="f"` || v(p.Metadata["Owner"]) != "team-"+n {
			t.Errorf("%s: properties %s %s %s %s %v", n, v(p.ContentType), v(p.ContentEncoding), v(p.CacheControl), v(p.ContentDisposition), p.Metadata)
		}
		if !bytes.Equal(p.ContentMD5, sp0.ContentMD5) || len(p.ContentMD5) == 0 {
			t.Errorf("%s: md5 %x vs %x", n, p.ContentMD5, sp0.ContentMD5)
		}
	}
	if _, err := dst.Blob.DownloadStream(ctx, "unselected", "x", nil); err == nil {
		t.Error("unselected container copied")
	}
	se, de := entities(t, src.Table, "Mixed"), entities(t, dst.Table, "Mixed")
	if len(de) != 25 {
		t.Fatalf("dest has %d entities", len(de))
	}
	for k, v := range se {
		if de[k] != v {
			t.Errorf("entity %s:\n src %s\n dst %s", k, v, de[k])
		}
	}

	// Second run: everything unchanged is skipped; a changed blob is copied again.
	stats, err = Run(ctx, src, dst, o, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if s := stats[0]; s.Skipped != 4 || s.Bytes != 0 {
		t.Errorf("second run: %+v", s)
	}
	src.Blob.UploadBuffer(ctx, "docs", "a/small.txt", []byte("HELLO"), nil)
	stats, _ = Run(ctx, src, dst, o, io.Discard)
	if s := stats[0]; s.Skipped != 3 || s.Bytes != 5 {
		t.Errorf("third run: %+v", s)
	}
	if got, _ := read(t, dst.Blob, "docs", "a/small.txt"); string(got) != "HELLO" {
		t.Errorf("changed blob not overwritten: %q", got)
	}

	// --all picks up the unselected container too.
	if _, err := Run(ctx, src, dst, Options{All: true, Concurrency: 2}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got, _ := read(t, dst.Blob, "docs", "b/other.txt"); string(got) != "other" {
		t.Error("--all did not copy b/other.txt")
	}
	if got, _ := read(t, dst.Blob, "unselected", "x"); string(got) != "x" {
		t.Error("--all did not copy unselected/x")
	}
}

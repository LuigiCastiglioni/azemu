package pull

import (
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	for _, c := range []struct {
		o    Options
		want string // substring of the error, "" = valid
	}{
		{Options{Concurrency: 8}, "nothing selected"},
		{Options{Concurrency: 8, Prefix: "p", Tables: []string{"t"}}, "--prefix"},
		{Options{Concurrency: 8, All: true, Tables: []string{"t"}}, "cannot be combined"},
		{Options{Concurrency: 0, All: true}, "--concurrency"},
		{Options{Concurrency: 8, All: true}, ""},
		{Options{Concurrency: 1, Containers: []string{"a"}, Prefix: "x"}, ""},
		{Options{Concurrency: 1, Tables: []string{"t"}}, ""},
	} {
		err := c.o.Validate()
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%+v: got %v, want %q", c.o, err, c.want)
		}
	}
}

func TestSameBlob(t *testing.T) {
	a, b := []byte{1, 2}, []byte{3, 4}
	for _, c := range []struct {
		name string
		ss   int64
		sm   []byte
		ds   int64
		dm   []byte
		skip bool
	}{
		{"identical", 5, a, 5, []byte{1, 2}, true},
		{"size differs", 5, a, 6, a, false},
		{"md5 differs", 5, a, 5, b, false},
		{"source md5 missing", 5, nil, 5, nil, false},
		{"dest md5 missing", 5, a, 5, nil, false},
	} {
		if got := sameBlob(c.ss, c.sm, c.ds, c.dm); got != c.skip {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}

func TestStripEntity(t *testing.T) {
	raw := `{"odata.type":"a.t","odata.id":"x","odata.etag":"W/\"1\"","odata.editLink":"t(1)","PartitionKey":"p","RowKey":"r",` +
		`"Timestamp@odata.type":"Edm.DateTime","Timestamp":"2024-01-01T00:00:00Z","Big@odata.type":"Edm.Int64","Big":"9007199254740993",` +
		`"D":1.0,"N":"NaN","N@odata.type":"Edm.Double"}`
	out, key, err := stripEntity([]byte(raw))
	if err != nil || key != "p/r" {
		t.Fatalf("%v %q", err, key)
	}
	s := string(out)
	for _, bad := range []string{"odata.etag", `"odata.type":`, "Timestamp", "editLink"} {
		if strings.Contains(s, bad) {
			t.Errorf("%s survived: %s", bad, s)
		}
	}
	for _, want := range []string{`"Big@odata.type":"Edm.Int64"`, `"Big":"9007199254740993"`, `"D":1.0`, `"N@odata.type":"Edm.Double"`} {
		if !strings.Contains(s, want) {
			t.Errorf("%s missing: %s", want, s)
		}
	}
}

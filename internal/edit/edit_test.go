package edit

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// The worked example of FMSG-008.
const (
	specSubject = "I'll see you their at 5pm"
	specTarget  = "I'll see you there at 5pm"
	specDocHex  = "19000000" + "000000000010000000" + "01020000007265" + "001200000007000000"
)

func TestApplySpecExample(t *testing.T) {
	doc, _ := hex.DecodeString(specDocHex)
	if len(doc) != 29 {
		t.Fatalf("doc is %d bytes, want 29", len(doc))
	}
	got, err := Apply(doc, []byte(specSubject), -1)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != specTarget {
		t.Fatalf("got %q", got)
	}
}

func TestDiffRoundTrip(t *testing.T) {
	long := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 40)
	cases := []struct{ name, subject, target string }{
		{"spec example", specSubject, specTarget},
		{"identical", long, long},
		{"typo in long text", long, strings.Replace(long, "lazy", "sleepy", 1)},
		{"append", long, long + "And then some."},
		{"prepend", long, "Well. " + long},
		{"truncate", long, long[:100]},
		{"to empty", long, ""},
		{"from empty", "", "hello"},
		{"both empty", "", ""},
		{"unrelated", "abc", "xyz"},
		{"multi-byte", "naïve café — 你好", "naive café — 你好!"},
		{"repeated bytes", strings.Repeat("a", 50), strings.Repeat("a", 30)},
		{"repeated bytes grow", strings.Repeat("a", 30), strings.Repeat("a", 50)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc := Diff([]byte(c.subject), []byte(c.target))
			got, err := Apply(doc, []byte(c.subject), -1)
			if err != nil {
				t.Fatalf("apply: %v (doc %x)", err, doc)
			}
			if string(got) != c.target {
				t.Fatalf("got %q, want %q", got, c.target)
			}
			if max := 4 + 5 + len(c.target); len(doc) > max {
				t.Fatalf("doc is %d bytes, larger than a full replacement of %d", len(doc), max)
			}
		})
	}
}

func TestDiffIsCompact(t *testing.T) {
	long := strings.Repeat("x", 100000)
	doc := Diff([]byte(long+"teh end"), []byte(long+"the end"))
	if len(doc) > 64 {
		t.Fatalf("doc is %d bytes for a two-byte change", len(doc))
	}
}

func TestCheckMalformed(t *testing.T) {
	subject := []byte("0123456789")
	valid, _ := hex.DecodeString("03000000" + "000200000003000000")
	if _, err := Check(valid, len(subject)); err != nil {
		t.Fatalf("valid doc rejected: %v", err)
	}
	cases := map[string]string{
		"empty":                   "",
		"short size":              "030000",
		"unknown op":              "01000000" + "02" + "01000000" + "61",
		"truncated copy":          "03000000" + "0002000000030000",
		"copy past end":           "03000000" + "000800000003000000",
		"copy offset overflow":    "01000000" + "00ffffffff01000000",
		"zero-length copy":        "00000000" + "000000000000000000",
		"zero-length insert":      "00000000" + "0100000000",
		"insert past end":         "05000000" + "0105000000" + "6162",
		"sum below result size":   "09000000" + "000200000003000000",
		"sum above result size":   "01000000" + "000200000003000000",
		"result size without ops": "01000000",
		"trailing byte":           "03000000" + "000200000003000000" + "00",
		"truncated insert header": "01000000" + "010100",
		"insert length wraps":     "01000000" + "01ffffffff" + "61",
	}
	for name, h := range cases {
		doc, err := hex.DecodeString(h)
		if err != nil {
			t.Fatalf("%s: bad hex", name)
		}
		if _, err := Check(doc, len(subject)); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: got %v, want ErrMalformed", name, err)
		}
		if _, err := Apply(doc, subject, -1); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: apply got %v, want ErrMalformed", name, err)
		}
	}
}

func TestEmptyResult(t *testing.T) {
	got, err := Apply([]byte{0, 0, 0, 0}, []byte("anything"), -1)
	if err != nil || len(got) != 0 {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestApplyLimit(t *testing.T) {
	subject := bytes.Repeat([]byte("a"), 1000)
	// Twenty copies of the whole subject: 184 bytes describing 20000.
	doc := []byte{0x20, 0x4e, 0, 0}
	for i := 0; i < 20; i++ {
		doc = appendCopy(doc, 0, 1000)
	}
	if _, err := Apply(doc, subject, 5000); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v, want ErrTooLarge", err)
	}
	got, err := Apply(doc, subject, 20000)
	if err != nil || len(got) != 20000 {
		t.Fatalf("got %d bytes, %v", len(got), err)
	}
}

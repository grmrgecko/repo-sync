package mirror

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// TestReadStanza verifies stanza splitting, continuation joining, and EOF
// handling without a trailing newline.
func TestReadStanza(t *testing.T) {
	input := "Package: hello\nSHA256:\n abc 10 path/one\n def 20 path/two\n\nPackage: world\nSize: 42"
	cr := newControlReader(strings.NewReader(input))

	st, err := cr.readStanza()
	if err != nil {
		t.Fatal(err)
	}
	if st.Get("Package") != "hello" {
		t.Errorf("Package = %q, want hello", st.Get("Package"))
	}
	if want := "\nabc 10 path/one\ndef 20 path/two"; st.Get("SHA256") != want {
		t.Errorf("SHA256 = %q, want %q", st.Get("SHA256"), want)
	}

	st, err = cr.readStanza()
	if err != nil {
		t.Fatal(err)
	}
	if st.Get("Package") != "world" || st.Get("Size") != "42" {
		t.Errorf("second stanza = %v", st)
	}

	if _, err = cr.readStanza(); !errors.Is(err, io.EOF) {
		t.Errorf("expected EOF, got %v", err)
	}
}

// TestStanzaGet verifies fallback field names cover casing differences.
func TestStanzaGet(t *testing.T) {
	st := stanza{"MD5Sum": "abc"}
	if got := st.Get("MD5sum", "MD5Sum"); got != "abc" {
		t.Errorf("got %q, want abc", got)
	}
	if got := st.Get("Missing"); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// TestStripClearsign verifies payload extraction and dash unescaping from a
// clearsigned document, and passthrough of unsigned input.
func TestStripClearsign(t *testing.T) {
	signed := "-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA256\n\nOrigin: Test\n- Dashed: line\n-----BEGIN PGP SIGNATURE-----\nabc\n-----END PGP SIGNATURE-----\n"
	got := string(stripClearsign([]byte(signed)))
	if !strings.Contains(got, "Origin: Test\n") {
		t.Errorf("payload missing Origin: %q", got)
	}
	if !strings.Contains(got, "Dashed: line") || strings.Contains(got, "- Dashed") {
		t.Errorf("dash escaping not reversed: %q", got)
	}
	if strings.Contains(got, "PGP") {
		t.Errorf("armor not removed: %q", got)
	}

	plain := "Origin: Test\n"
	if string(stripClearsign([]byte(plain))) != plain {
		t.Error("unsigned input was modified")
	}
}

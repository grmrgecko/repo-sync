package mirror

import (
	"bufio"
	"io"
	"strings"
)

// stanza is one RFC 822 style field block from an apt control file.
type stanza map[string]string

// Get returns the first matching field, trying each name in order to cope
// with casing differences between repositories.
func (s stanza) Get(names ...string) string {
	for _, name := range names {
		if v, ok := s[name]; ok {
			return v
		}
	}
	return ""
}

// controlReader reads stanzas from an apt control file.
type controlReader struct {
	br *bufio.Reader
}

// newControlReader wraps a reader for stanza-by-stanza parsing.
func newControlReader(r io.Reader) *controlReader {
	return &controlReader{br: bufio.NewReaderSize(r, 64*1024)}
}

// readStanza parses the next stanza, returning io.EOF when the input is
// exhausted. Continuation lines are joined with newlines so multi-line
// checksum fields keep one entry per line.
func (r *controlReader) readStanza() (stanza, error) {
	st := stanza{}
	var last string
	for {
		line, err := r.br.ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, err
		}
		done := err == io.EOF && line == ""
		line = strings.TrimRight(line, "\r\n")
		switch {
		case done || strings.TrimSpace(line) == "":
			if len(st) > 0 {
				return st, nil
			}
			if done {
				return nil, io.EOF
			}
		case line[0] == ' ' || line[0] == '\t':
			if last != "" {
				st[last] += "\n" + strings.TrimSpace(line)
			}
		default:
			name, value, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			last = name
			st[name] = strings.TrimSpace(value)
		}
	}
}

// stripClearsign extracts the signed payload from a clearsigned InRelease
// document, returning the input unchanged when it is not clearsigned.
func stripClearsign(data []byte) []byte {
	const begin = "-----BEGIN PGP SIGNED MESSAGE-----"
	const sig = "-----BEGIN PGP SIGNATURE-----"
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	idx := strings.Index(text, begin)
	if idx < 0 {
		return data
	}
	text = text[idx+len(begin):]

	// Skip the armor headers up to the first blank line. Without one the
	// document is malformed, so treat it as not clearsigned rather than
	// returning the armor headers as payload.
	i := strings.Index(text, "\n\n")
	if i < 0 {
		return data
	}
	text = text[i+2:]
	if i := strings.Index(text, sig); i >= 0 {
		text = text[:i]
	}

	// Reverse the dash escaping applied by the signer.
	var b strings.Builder
	for _, line := range strings.Split(text, "\n") {
		b.WriteString(strings.TrimPrefix(line, "- "))
		b.WriteString("\n")
	}
	return []byte(b.String())
}

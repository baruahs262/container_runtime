package image

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Instruction is one parsed Buildfile line.
type Instruction struct {
	Line int
	Cmd  string   // upper-case keyword
	Raw  string   // arguments as written
	Args []string // exec-form or word-split arguments
	JSON bool     // Args came from a JSON array
}

func (i Instruction) String() string { return i.Cmd + " " + i.Raw }

var knownCmds = map[string]bool{
	"FROM": true, "RUN": true, "COPY": true, "ADD": true, "ENV": true, "WORKDIR": true,
	"CMD": true, "ENTRYPOINT": true, "LABEL": true,
}

// ParseBuildfile reads a Dockerfile-style build recipe. It supports
// comments, backslash line continuations, and exec (JSON) or shell forms.
func ParseBuildfile(r io.Reader) ([]Instruction, error) {
	var out []Instruction
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	var buf strings.Builder
	start, lineNo := 0, 0
	flush := func() error {
		s := strings.TrimSpace(buf.String())
		buf.Reset()
		if s == "" {
			return nil
		}
		kw, rest, _ := strings.Cut(s, " ")
		if i := strings.IndexAny(kw, "\t"); i >= 0 {
			kw, rest = kw[:i], kw[i+1:]+" "+rest
		}
		kw = strings.ToUpper(kw)
		rest = strings.TrimSpace(rest)
		if !knownCmds[kw] {
			return fmt.Errorf("line %d: unsupported instruction %q", start, kw)
		}
		if rest == "" {
			return fmt.Errorf("line %d: %s needs arguments", start, kw)
		}
		in := Instruction{Line: start, Cmd: kw, Raw: rest}
		if strings.HasPrefix(rest, "[") {
			if err := json.Unmarshal([]byte(rest), &in.Args); err == nil {
				in.JSON = true
			}
		}
		if !in.JSON {
			in.Args = splitWords(rest)
		}
		if kw == "ADD" {
			in.Cmd = "COPY" // local sources only; no URLs or auto-extract
		}
		out = append(out, in)
		return nil
	}
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if buf.Len() == 0 {
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			start = lineNo
		} else if strings.HasPrefix(trimmed, "#") {
			continue // comments inside a continued instruction
		}
		if strings.HasSuffix(line, "\\") {
			buf.WriteString(strings.TrimSuffix(line, "\\"))
			buf.WriteByte(' ')
			continue
		}
		buf.WriteString(line)
		if err := flush(); err != nil {
			return nil, err
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty Buildfile")
	}
	if out[0].Cmd != "FROM" {
		return nil, fmt.Errorf("line %d: first instruction must be FROM", out[0].Line)
	}
	for _, in := range out[1:] {
		if in.Cmd == "FROM" {
			return nil, fmt.Errorf("line %d: multi-stage builds are not supported", in.Line)
		}
	}
	return out, nil
}

// splitWords splits on whitespace, honouring single and double quotes and
// backslash escapes.
func splitWords(s string) []string {
	var out []string
	var cur strings.Builder
	var quote rune
	inWord, esc := false, false
	for _, r := range s {
		switch {
		case esc:
			cur.WriteRune(r)
			esc = false
		case r == '\\' && quote != '\'':
			esc, inWord = true, true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote, inWord = r, true
		case r == ' ' || r == '\t':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out
}

// parseKV handles ENV and LABEL: "K=V K2='v 2'" or the legacy "K value".
func parseKV(in Instruction) ([][2]string, error) {
	if len(in.Args) == 0 {
		return nil, fmt.Errorf("line %d: %s needs arguments", in.Line, in.Cmd)
	}
	if !strings.Contains(in.Args[0], "=") {
		k, v, _ := strings.Cut(in.Raw, " ")
		return [][2]string{{k, strings.TrimSpace(v)}}, nil
	}
	var out [][2]string
	for _, w := range in.Args {
		k, v, ok := strings.Cut(w, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("line %d: bad %s pair %q", in.Line, in.Cmd, w)
		}
		out = append(out, [2]string{k, v})
	}
	return out, nil
}

// commandArgs returns exec-form args, or wraps shell form in /bin/sh -c.
func commandArgs(in Instruction) []string {
	if in.JSON {
		return in.Args
	}
	return []string{"/bin/sh", "-c", in.Raw}
}

package image

import (
	"fmt"
	"strings"
)

// Ref is a parsed image reference: [registry/]repo[:tag][@digest].
type Ref struct {
	Name   string // as the user wrote it minus tag/digest, e.g. "alpine", "ghcr.io/org/app"
	Tag    string
	Digest string
}

// ParseRef parses s, defaulting the tag to "latest".
func ParseRef(s string) (Ref, error) {
	if s == "" {
		return Ref{}, fmt.Errorf("empty image reference")
	}
	var r Ref
	if i := strings.Index(s, "@"); i >= 0 {
		r.Digest = s[i+1:]
		s = s[:i]
		if !strings.HasPrefix(r.Digest, "sha256:") {
			return Ref{}, fmt.Errorf("unsupported digest %q", r.Digest)
		}
	}
	// A colon after the last slash separates the tag (a colon before it
	// belongs to a registry host:port).
	slash := strings.LastIndex(s, "/")
	if i := strings.LastIndex(s, ":"); i > slash {
		r.Tag = s[i+1:]
		s = s[:i]
	}
	if r.Tag == "" && r.Digest == "" {
		r.Tag = "latest"
	}
	if s == "" || strings.ContainsAny(s, " \t\n") || s != strings.ToLower(s) {
		return Ref{}, fmt.Errorf("invalid image name %q", s)
	}
	r.Name = s
	return r, nil
}

// String renders the reference in canonical short form.
func (r Ref) String() string {
	s := r.Name
	if r.Tag != "" {
		s += ":" + r.Tag
	}
	if r.Digest != "" {
		s += "@" + r.Digest
	}
	return s
}

// Registry splits the name into registry host and repository path using
// Docker's conventions: a first component containing "." or ":" or equal
// to "localhost" is a host, otherwise Docker Hub, where single-component
// names live under "library/".
func (r Ref) Registry() (host, repo string) {
	first, rest, ok := strings.Cut(r.Name, "/")
	if ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		host, repo = first, rest
	} else {
		host, repo = "registry-1.docker.io", r.Name
	}
	if host == "docker.io" || host == "index.docker.io" {
		host = "registry-1.docker.io"
	}
	if host == "registry-1.docker.io" && !strings.Contains(repo, "/") {
		repo = "library/" + repo
	}
	return host, repo
}

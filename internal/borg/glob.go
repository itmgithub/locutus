package borg

import (
	"path/filepath"
	"regexp"
	"strings"
)

// safeBracket is a bracket expression ([abc], [!0-9]) that may be left
// unquoted for the shell: only letters, digits and a few harmless characters,
// so it can never contain spaces or shell syntax.
var safeBracket = regexp.MustCompile(`^\[[!^]?[A-Za-z0-9._:@%+,=-]+\]`)

// Backup paths may contain shell wildcards (*, ? and [...]), as in a crontab
// entry. borg itself does not expand them, so Locutus does: in the container
// for local clients, and through the remote shell for remote clients.

// hasGlob reports whether p contains wildcard characters.
func hasGlob(p string) bool { return strings.ContainsAny(p, "*?[") }

// ExpandPaths expands wildcards the way a POSIX shell does: "*" and "?" do
// not match a leading dot, and a pattern without matches is kept unchanged so
// that borg reports it as not found.
func ExpandPaths(paths []string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, p := range paths {
		if !hasGlob(p) {
			add(p)
			continue
		}
		matches, err := filepath.Glob(p)
		found := false
		if err == nil {
			for _, m := range matches {
				if !hiddenMismatch(p, m) {
					add(m)
					found = true
				}
			}
		}
		if !found {
			add(p)
		}
	}
	return out
}

// hiddenMismatch reports whether a wildcard matched a dot file that a shell
// would skip (the pattern segment does not start with a dot itself).
func hiddenMismatch(pattern, match string) bool {
	ps := strings.Split(pattern, "/")
	ms := strings.Split(match, "/")
	if len(ps) != len(ms) {
		return false
	}
	for i := range ps {
		if hasGlob(ps[i]) && !strings.HasPrefix(ps[i], ".") && strings.HasPrefix(ms[i], ".") {
			return true
		}
	}
	return false
}

// ShellQuoteGlob quotes p for a POSIX shell but leaves the wildcards *, ?
// and safe [...] expressions unquoted, so that the remote shell expands them.
// Only these wildcard characters are ever outside quotes. The result of a
// pathname expansion is never parsed again by the shell, so a file name such
// as "$(rm -rf ~)" or "; id" just becomes a path argument (and all paths come
// after "--", so a name like "-x" cannot be taken for an option either).
func ShellQuoteGlob(p string) string {
	if !hasGlob(p) {
		return ShellQuote(p)
	}
	var b strings.Builder
	lit := strings.Builder{}
	flush := func() {
		if lit.Len() > 0 {
			b.WriteString(ShellQuote(lit.String()))
			lit.Reset()
		}
	}
	for i := 0; i < len(p); i++ {
		switch c := p[i]; c {
		case '*', '?':
			flush()
			b.WriteByte(c)
		case '[':
			if m := safeBracket.FindString(p[i:]); m != "" {
				flush()
				b.WriteString(m)
				i += len(m) - 1
			} else {
				lit.WriteByte(c)
			}
		default:
			lit.WriteByte(c)
		}
	}
	flush()
	return b.String()
}

package cli

import (
	"path"
	"strings"
)

// Scope filtering: an invariant is served only to the paths it binds.
//
// Until this existed, `scope` was decoration. Delivery was decided entirely by
// `git log -- <path>` — the commits that touched the file — and every invariant
// in those commits was shown whoever opened whatever. So a rule scoped to
// internal/auth/** was served to an agent editing README.md, purely because one
// commit had touched both, and the scope line sat there saying otherwise.
//
// This narrows an earlier rule of this package — that injected context passes
// through verbatim, with only bookkeeping stripped. The exception is deliberate:
// a rule the reader is told does not apply to them is not reasoning, it is
// noise, and it costs the same context as a rule that does apply.
//
// An invariant with no scope still reaches everyone. That is what an empty scope
// means, and the extraction prompt tells the model exactly that.

// dropOutOfScope removes invariant entries whose scope does not cover path.
// Everything else in the message — the author's words, why, rejections — is
// untouched.
func dropOutOfScope(msg, path string) string {
	if !strings.Contains(msg, invariantLine) && !strings.Contains(msg, legacyInvariantLine) {
		return msg
	}
	lines := strings.Split(msg, "\n")
	var out []string
	for i := 0; i < len(lines); {
		scopes, span, ok := invariantAt(lines, i)
		if !ok {
			out = append(out, lines[i])
			i++
			continue
		}
		if len(scopes) == 0 || scopeMatches(scopes, path) {
			out = append(out, lines[i:i+span]...)
		}
		i += span
	}
	// Dropping the last entry can leave the block holding nothing but its own
	// tags, which announces reasoning that is not there.
	return tidy(out)
}

const (
	invariantLine       = "invariant: "
	legacyInvariantLine = "Invariant: "
	scopeLine           = "scope:"
)

// invariantAt reports whether an invariant entry starts at lines[i], returning
// its declared scopes and how many lines it spans.
//
// An entry runs until a blank line or the next unindented line, which is the
// same grammar the record is written with: a key at column zero, continuations
// indented. Legacy entries are single wrapped paragraphs with the scope in
// trailing parentheses, and are recognised the same way.
func invariantAt(lines []string, i int) (scopes []string, span int, ok bool) {
	first := lines[i]
	if strings.TrimSpace(first) == "" {
		return nil, 0, false
	}
	legacy := strings.HasPrefix(first, legacyInvariantLine)
	if !strings.HasPrefix(first, invariantLine) && !legacy {
		return nil, 0, false
	}
	span = 1
	var body []string
	body = append(body, first)
	for j := i + 1; j < len(lines); j++ {
		l := lines[j]
		if strings.TrimSpace(l) == "" {
			break
		}
		// A new unindented key ends this entry. Legacy records are not indented at
		// all, so for those only another prefixed line can end it.
		if legacy {
			if hasAnyPrefix(strings.TrimSpace(l), append([]string{legacyInvariantLine, "Rejected: "}, tailMarkers...)) {
				break
			}
		} else if l[0] != ' ' && l[0] != '\t' {
			break
		}
		body = append(body, l)
		span++
	}
	return scopesIn(body, legacy), span, true
}

// scopesIn pulls the paths an entry declares: a "scope:" subfield in the block
// format, or a trailing parenthesised list in the legacy one.
func scopesIn(body []string, legacy bool) []string {
	joined := strings.Join(body, " ")
	if legacy {
		// "Invariant: text … (internal/auth/**, internal/cli/**)"
		open := strings.LastIndex(joined, "(")
		close := strings.LastIndex(joined, ")")
		if open < 0 || close < open {
			return nil
		}
		return splitScopes(joined[open+1 : close])
	}
	i := strings.Index(joined, scopeLine)
	if i < 0 {
		return nil
	}
	return splitScopes(joined[i+len(scopeLine):])
}

func splitScopes(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		p = strings.TrimSpace(p)
		// A legacy scope list can hold prose ("every error path degrades…"), which
		// is not a path and must not be read as one. A path has a separator, a
		// wildcard or a file extension.
		if p == "" || !looksLikePath(p) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// looksLikePath keeps prose out of a legacy scope list.
//
// It has to be strict in one direction: a rule read as scoped to something that
// is not a path matches nothing and disappears for every reader. A trailing
// parenthetical aside — "(e.g. Redis)" — is the case that would do it, so a bare
// word and a token ending in a dot are not paths. A separator or a wildcard is
// decisive; otherwise the token needs a plausible file extension.
func looksLikePath(p string) bool {
	if strings.ContainsAny(p, "/*") {
		return true
	}
	i := strings.LastIndexByte(p, '.')
	if i <= 0 || i == len(p)-1 {
		return false
	}
	ext := p[i+1:]
	if len(ext) > 5 {
		return false
	}
	for _, r := range ext {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// scopeMatches reports whether any glob covers path.
func scopeMatches(scopes []string, target string) bool {
	target = strings.TrimPrefix(target, "./")
	for _, s := range scopes {
		if globCovers(s, target) {
			return true
		}
	}
	return false
}

// globCovers matches one scope glob against a repo-relative path.
//
// path.Match has no "**", which is the notation every record uses, so the
// double star is handled here: a trailing "/**" is a directory prefix, and an
// inner "**" splits into a prefix and a suffix. A pattern with no separator at
// all is matched against the base name, so "*.go" behaves the way anyone writing
// it expects.
func globCovers(pattern, target string) bool {
	pattern = strings.TrimSuffix(strings.TrimPrefix(pattern, "./"), "/")
	switch {
	case pattern == "" || pattern == "*" || pattern == "**":
		return true
	case strings.HasSuffix(pattern, "/**"):
		dir := strings.TrimSuffix(pattern, "/**")
		return target == dir || strings.HasPrefix(target, dir+"/")
	case strings.Contains(pattern, "**"):
		head, tail, _ := strings.Cut(pattern, "**")
		return strings.HasPrefix(target, head) && strings.HasSuffix(target, strings.TrimPrefix(tail, "/"))
	case !strings.Contains(pattern, "/"):
		if ok, _ := path.Match(pattern, path.Base(target)); ok {
			return true
		}
		// A bare directory name binds everything under it.
		return target == pattern || strings.HasPrefix(target, pattern+"/")
	}
	if ok, _ := path.Match(pattern, target); ok {
		return true
	}
	// A directory named without a wildcard binds its contents.
	return strings.HasPrefix(target, pattern+"/")
}

// tidy drops a <git-cairn> block left with no fields, and any trailing blank
// lines, after filtering.
func tidy(lines []string) string {
	var out []string
	for i := 0; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != openTag {
			out = append(out, lines[i])
			continue
		}
		end, empty := blockEnd(lines, i)
		if empty {
			i = end // skip the whole block, tags included
			continue
		}
		out = append(out, lines[i])
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

// blockEnd finds the closing tag from an opening one, and reports whether the
// block holds no fields.
func blockEnd(lines []string, open int) (end int, empty bool) {
	empty = true
	for j := open + 1; j < len(lines); j++ {
		t := strings.TrimSpace(lines[j])
		if t == closeTag {
			return j, empty
		}
		if t != "" {
			empty = false
		}
	}
	return len(lines) - 1, empty
}

const (
	openTag  = "<git-cairn>"
	closeTag = "</git-cairn>"
)

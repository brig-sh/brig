package config

import (
	"fmt"
	"os"
	"strings"
)

// Expand resolves env expressions in a config value.
//
//	${VAR}            the value of VAR
//	${VAR:-default}   default if VAR is unset OR empty
//	${VAR-default}    default only if VAR is unset
//	$$                a literal $
//
// Braces are required. A bare $VAR is not an expression, because $ turns up in
// real values -- an image tag, a shell snippet -- and guessing which dollars
// are ours would make those values unwriteable.
//
// A variable that is unset and has no default is an error rather than an empty
// string. Values here are policy: skills.deny resolving quietly to nothing
// would be a fail-open, and the whole point of a deny list is that it does not
// fail open.
//
// Expansion happens at load time and in memory only. The file keeps the
// expression, never the expansion, so a referenced token is never written to
// disk. It is still not the credentials path: a value pulled in this way is a
// value, and secrets stay declared as references (fromEnv:, from:) on keys of
// Kind EnvVar, which are never expanded.
//
// lookup defaults to os.LookupEnv. One level only: a default may not itself
// contain an expression.
//
// Two limits worth knowing, both from the closing brace being found by a plain
// scan for the first "}". A default cannot contain "}" -- write the literal
// outside the expression instead of ${X:-{"a":1}}. And ":-" is recognised
// before "-", so ${VAR-a:-b} reads as the name "VAR-a" and is rejected, where a
// shell would read VAR with the default "a:-b". Rejecting beats guessing: the
// alternative is silently resolving something the author did not write.
func Expand(s string, lookup func(string) (string, bool)) (string, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	// Nothing to do, which is the common case: skip the whole scan.
	if !strings.Contains(s, "$") {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '$' {
			b.WriteByte(s[i])
			i++
			continue
		}
		if i+1 < len(s) && s[i+1] == '$' {
			b.WriteByte('$')
			i += 2
			continue
		}
		if i+1 >= len(s) || s[i+1] != '{' {
			// A dollar that opens nothing is just a dollar.
			b.WriteByte('$')
			i++
			continue
		}
		end := strings.IndexByte(s[i+2:], '}')
		if end < 0 {
			return "", fmt.Errorf("unterminated ${ in %q", s)
		}
		v, err := resolveExpr(s[i+2:i+2+end], lookup)
		if err != nil {
			return "", err
		}
		b.WriteString(v)
		i += 2 + end + 1
	}
	return b.String(), nil
}

// resolveExpr resolves the inside of one ${...}.
func resolveExpr(expr string, lookup func(string) (string, bool)) (string, error) {
	name, def := expr, ""
	hasDefault, emptyCounts := false, false
	// ":-" is checked before "-" so a default may itself contain a dash, and a
	// dash alone reads as the shell's ${VAR-default}: "${SET-a-b}" is SET with
	// the default "a-b", not a variable called "SET-a".
	if before, after, found := strings.Cut(expr, ":-"); found {
		name, def = before, after
		hasDefault, emptyCounts = true, true
	} else if before, after, found := strings.Cut(expr, "-"); found {
		name, def = before, after
		hasDefault = true
	}
	if !validVarName(name) {
		return "", fmt.Errorf("${%s}: %q is not a variable name", expr, name)
	}
	if strings.Contains(def, "${") {
		return "", fmt.Errorf("${%s}: a default may not contain an expression", expr)
	}
	v, ok := lookup(name)
	if ok && (v != "" || !emptyCounts) {
		return v, nil
	}
	if hasDefault {
		return def, nil
	}
	return "", fmt.Errorf("%s is not set and has no default; export it or write ${%s:-...}", name, name)
}

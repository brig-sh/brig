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
// Braces are required: $ turns up in real values, so guessing which dollars are
// ours would make those values unwriteable.
//
// An unset variable with no default is an error, not an empty string. These
// values are policy, and skills.deny resolving quietly to nothing is a
// fail-open.
//
// The file keeps the expression and never the expansion, so a referenced token
// is never written to disk. This is still not the credentials path: what comes
// back here is a value, and secrets stay references on EnvVar keys.
//
// lookup defaults to os.LookupEnv. A default may not contain "}" or a nested
// expression, both because the closing brace is the first one found.
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
//
// ":-" is cut before "-" so a default may contain a dash: ${SET-a-b} is SET with
// the default "a-b". The cost is that ${VAR-a:-b} reads as the name "VAR-a" and
// is rejected, where a shell would read VAR defaulting to "a:-b". Rejecting
// beats guessing at which the author meant.
func resolveExpr(expr string, lookup func(string) (string, bool)) (string, error) {
	name, def := expr, ""
	hasDefault, emptyCounts := false, false
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

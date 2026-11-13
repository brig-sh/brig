package graphql

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"
	"unicode/utf8"
)

// Rules is what a GraphQL request may do. A list that is nil allows nothing,
// and "*" in Queries or Mutations allows any field.
type Rules struct {
	// Queries lists the top-level fields of Query a query may select.
	Queries []string
	// Mutations lists the mutations a request may call.
	Mutations []string
	// Repos lists owner/name globs. When set, every repository(owner:,
	// name:) field at the root of a query must name a match. It limits where
	// a query starts, not where it goes: an edge such as Issue.parent or
	// Repository.forks reaches other repositories. Mutations name node IDs,
	// not repositories, so Repos does not limit them.
	Repos []string
}

// Request is the JSON body of a GraphQL request over HTTP.
type Request struct {
	Query         string
	OperationName string
	Variables     map[string]any
}

// ReadRequest decodes body as a GraphQL request. It refuses batches, keys
// other than query, operationName and variables, and duplicate keys at any
// depth, so the server cannot read a request the broker did not check.
func ReadRequest(body []byte) (*Request, error) {
	// encoding/json reads a bad byte as U+FFFD, and a server may not.
	if !utf8.Valid(body) {
		return nil, fmt.Errorf("body is not valid UTF-8")
	}
	if err := noDuplicateKeys(body); err != nil {
		return nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("body must be one JSON object; batches are not supported")
	}
	req := &Request{}
	for k, v := range raw {
		switch k {
		case "query":
			if err := json.Unmarshal(v, &req.Query); err != nil {
				return nil, fmt.Errorf("query must be a string")
			}
		case "operationName":
			var s *string
			if err := json.Unmarshal(v, &s); err != nil {
				return nil, fmt.Errorf("operationName must be a string")
			}
			if s != nil {
				req.OperationName = *s
			}
		case "variables":
			d := json.NewDecoder(bytes.NewReader(v))
			d.UseNumber()
			if err := d.Decode(&req.Variables); err != nil {
				return nil, fmt.Errorf("variables must be a JSON object")
			}
		default:
			return nil, fmt.Errorf("unsupported key %q; use only query, operationName and variables", k)
		}
	}
	if req.Query == "" {
		return nil, fmt.Errorf("body has no query; set query to the GraphQL document")
	}
	return req, nil
}

// Repo is a repository a query names at its root.
type Repo struct{ Owner, Name string }

// String returns owner/name.
func (r Repo) String() string { return r.Owner + "/" + r.Name }

// Check reports whether req may go upstream under r, and returns the
// repositories its query names at its root. The caller must confirm they
// exist under those names, because GitHub resolves the old name of a renamed
// or transferred repository.
func Check(req *Request, r Rules) ([]Repo, error) {
	doc, err := Parse(req.Query)
	if err != nil {
		return nil, fmt.Errorf("invalid query: %w", err)
	}
	op, err := doc.Operation(req.OperationName)
	if err != nil {
		return nil, err
	}
	var allowed []string
	switch op.Kind {
	case "query":
		allowed = r.Queries
	case "mutation":
		allowed = r.Mutations
	default:
		return nil, fmt.Errorf("%s operations are not supported", op.Kind)
	}
	for _, f := range rootFields(doc, op.Selections) {
		if f == "__typename" {
			continue
		}
		if !slices.Contains(allowed, "*") && !slices.Contains(allowed, f) {
			return nil, fmt.Errorf("%s field %q is not in the allow list %s", op.Kind, f, list(allowed))
		}
	}
	if len(r.Repos) == 0 || op.Kind != "query" {
		return nil, nil
	}
	c := repoCheck{op: op, vars: req.Variables, repos: r.Repos}
	var repos []Repo
	for _, f := range rootSelections(doc, op.Selections) {
		if f.Name != "repository" {
			continue
		}
		repo, err := c.repository(f)
		if err != nil {
			return nil, err
		}
		repos = append(repos, repo)
	}
	return repos, nil
}

// rootFields returns the names of the fields an operation selects at its
// root, through fragments.
func rootFields(doc *Document, sels []Selection) []string {
	var out []string
	for _, f := range rootSelections(doc, sels) {
		out = append(out, f.Name)
	}
	return out
}

// rootSelections returns the fields an operation selects at its root,
// through fragments.
func rootSelections(doc *Document, sels []Selection) []*Field {
	var out []*Field
	seen := map[string]bool{}
	var walk func([]Selection)
	walk = func(sels []Selection) {
		for _, s := range sels {
			switch {
			case s.Field != nil:
				out = append(out, s.Field)
			case s.Inline != nil:
				walk(s.Inline.Selections)
			case !seen[s.Spread]:
				seen[s.Spread] = true
				walk(doc.Fragments[s.Spread].Selections)
			}
		}
	}
	walk(sels)
	return out
}

type repoCheck struct {
	op    *Operation
	vars  map[string]any
	repos []string
}

// repository refuses a root repository field unless it names owner and
// name in ASCII, and they match a glob. GitHub matches without case, and so
// does the glob, in ASCII alone: strings.ToLower folds the Kelvin sign to k.
func (c *repoCheck) repository(f *Field) (Repo, error) {
	var repo Repo
	for _, a := range f.Arguments {
		switch a.Name {
		case "owner", "name":
			s, err := c.str(a.Value)
			if err != nil {
				return Repo{}, fmt.Errorf("repository %s: %w", a.Name, err)
			}
			if !isASCII(s) {
				return Repo{}, fmt.Errorf("repository %s %q contains non-ASCII characters", a.Name, s)
			}
			if a.Name == "owner" {
				repo.Owner = s
			} else {
				repo.Name = s
			}
		}
	}
	if repo.Owner == "" || repo.Name == "" {
		return Repo{}, fmt.Errorf("repository field needs both owner and name")
	}
	for _, g := range c.repos {
		if ok, err := path.Match(asciiLower(g), asciiLower(repo.String())); err == nil && ok {
			return repo, nil
		}
	}
	return Repo{}, fmt.Errorf("repo %q is not in the allow list %s", repo, list(c.repos))
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// str resolves v to a string: a literal, or a variable from the request or
// its default.
func (c *repoCheck) str(v Value) (string, error) {
	switch v.Kind {
	case KindString:
		if v.Block {
			return "", fmt.Errorf("block strings are not supported here; use a quoted string")
		}
		return v.Raw, nil
	case KindVariable:
		if x, ok := c.vars[v.Raw]; ok {
			s, ok := x.(string)
			if !ok {
				return "", fmt.Errorf("variable $%s must be a string", v.Raw)
			}
			return s, nil
		}
		for _, d := range c.op.Variables {
			if d.Name == v.Raw {
				if d.Default == nil {
					return "", fmt.Errorf("variable $%s has no value; set it in variables", v.Raw)
				}
				return c.str(*d.Default)
			}
		}
		return "", fmt.Errorf("variable $%s is not declared in the operation", v.Raw)
	}
	return "", fmt.Errorf("value must be a string")
}

// noDuplicateKeys refuses JSON with an object that repeats a key.
func noDuplicateKeys(body []byte) error {
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	depth := 0
	var walk func() error
	walk = func() error {
		// Without a limit, 8 MiB of [ overflows the stack, which is fatal.
		depth++
		defer func() { depth-- }()
		if depth > maxDepth {
			return fmt.Errorf("body is nested more than %d levels deep", maxDepth)
		}
		t, err := d.Token()
		if err != nil {
			return fmt.Errorf("body is not valid JSON: %w", err)
		}
		switch t {
		case json.Delim('{'):
			keys := map[string]bool{}
			for d.More() {
				k, err := d.Token()
				if err != nil {
					return fmt.Errorf("body is not valid JSON: %w", err)
				}
				ks := k.(string)
				if keys[ks] {
					return fmt.Errorf("duplicate key %q", ks)
				}
				keys[ks] = true
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = d.Token()
			return err
		case json.Delim('['):
			for d.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = d.Token()
			return err
		}
		return nil
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := d.Token(); err == nil {
		return fmt.Errorf("body has more than one JSON value")
	}
	return nil
}

func list(l []string) string {
	if len(l) == 0 {
		return "[]"
	}
	return "[" + strings.Join(l, ", ") + "]"
}

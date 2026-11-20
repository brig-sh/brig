package graphql

import (
	"fmt"
	"strings"
)

// Node is a node ID that a mutation names, and where: the field, argument
// and input fields that hold it, such as addComment.input.subjectId.
type Node struct{ ID, Path string }

// isIDField reports whether an argument or input field holds node IDs.
// GitHub's schema names them id, ids, or with an Id or Ids suffix, such as
// subjectId and labelIds. clientMutationId is the one that names no node.
// inReplyTo, allowedActors and reviewers are the ID fields without the suffix.
func isIDField(name string) bool {
	switch name {
	case "clientMutationId":
		return false
	case "id", "ids", "inReplyTo", "allowedActors", "reviewers":
		return true
	}
	return strings.HasSuffix(name, "Id") || strings.HasSuffix(name, "Ids")
}

// notByID returns why an input field is refused under repos, or "". A field
// that names its target by name or by URL, such as
// CommittableBranch.repositoryNameWithOwner or AddSubIssueInput.subIssueUrl,
// cannot be looked up as a node ID, so it is refused rather than left
// unchecked beside an allowed ID.
func notByID(name string) string {
	switch {
	case strings.HasSuffix(name, "NameWithOwner"):
		return "names a repository by name; repos checks node IDs, so use one"
	case name == "url" || strings.HasSuffix(name, "Url") || strings.HasSuffix(name, "URL"):
		return "takes a URL, which repos cannot check; use a node ID field, or leave it out"
	}
	return ""
}

// otherHead returns why a headRefName is refused: "owner:branch" names a
// branch in a fork, which createPullRequest accepts beside a repositoryId in
// the listed repository.
func otherHead(path, name, ref string) error {
	if name == "headRefName" && strings.Contains(ref, ":") {
		return fmt.Errorf("%s.%s %q names a branch in another repository; repos checks node IDs, so use headRepositoryId", path, name, ref)
	}
	return nil
}

// nodes returns the node IDs in the arguments of each root field of a
// mutation, through input objects, lists, variables and their defaults. A
// field that names no node ID is refused: nothing ties it to a repository.
func (c *repoCheck) nodes(doc *Document) ([]Node, error) {
	var out []Node
	for _, f := range rootSelections(doc, c.op.Selections) {
		if f.Name == "__typename" {
			continue
		}
		n := len(out)
		for _, a := range f.Arguments {
			if why := notByID(a.Name); why != "" && a.Value.Kind != KindNull {
				return nil, fmt.Errorf("%s.%s %s", f.Name, a.Name, why)
			}
			var err error
			if out, err = c.walkValue(out, f.Name+"."+a.Name, isIDField(a.Name), a.Value); err != nil {
				return nil, err
			}
		}
		if len(out) == n {
			return nil, fmt.Errorf("mutation field %q names no node ID, so repos cannot check it", f.Name)
		}
	}
	return out, nil
}

// walkValue appends the node IDs in v. id says that v is the value of a field
// that holds node IDs: a string, a list of them, or null.
func (c *repoCheck) walkValue(out []Node, path string, id bool, v Value) ([]Node, error) {
	switch v.Kind {
	case KindVariable:
		if x, ok := c.vars[v.Raw]; ok {
			return walkJSON(out, path, id, x)
		}
		for _, d := range c.op.Variables {
			if d.Name == v.Raw {
				if d.Default == nil {
					return out, nil
				}
				return c.walkValue(out, path, id, *d.Default)
			}
		}
		return nil, fmt.Errorf("%s: variable $%s is not declared in the operation", path, v.Raw)
	case KindNull:
		return out, nil
	case KindList:
		for _, item := range v.List {
			var err error
			if out, err = c.walkValue(out, path, id, item); err != nil {
				return nil, err
			}
		}
		return out, nil
	case KindString:
		if id {
			if v.Block {
				return nil, fmt.Errorf("%s: block strings are not supported here; use a quoted string", path)
			}
			return append(out, Node{ID: v.Raw, Path: path}), nil
		}
		return out, nil
	case KindObject:
		if id {
			return nil, fmt.Errorf("%s must be a node ID string", path)
		}
		for _, f := range v.Fields {
			if why := notByID(f.Name); why != "" && f.Value.Kind != KindNull {
				return nil, fmt.Errorf("%s.%s %s", path, f.Name, why)
			}
			if f.Name == "headRefName" && f.Value.Kind != KindNull {
				// A value the broker cannot read, such as a block string,
				// is refused: GitHub would read it.
				ref, err := c.str(f.Value)
				if err != nil {
					return nil, fmt.Errorf("%s.%s: %w", path, f.Name, err)
				}
				if err := otherHead(path, f.Name, ref); err != nil {
					return nil, err
				}
			}
			var err error
			if out, err = c.walkValue(out, path+"."+f.Name, isIDField(f.Name), f.Value); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	if id {
		return nil, fmt.Errorf("%s must be a node ID string", path)
	}
	return out, nil
}

// walkJSON is walkValue for a variable's value, as encoding/json decodes it.
func walkJSON(out []Node, path string, id bool, x any) ([]Node, error) {
	switch x := x.(type) {
	case nil:
		return out, nil
	case string:
		if id {
			return append(out, Node{ID: x, Path: path}), nil
		}
		return out, nil
	case []any:
		for _, item := range x {
			var err error
			if out, err = walkJSON(out, path, id, item); err != nil {
				return nil, err
			}
		}
		return out, nil
	case map[string]any:
		if id {
			return nil, fmt.Errorf("%s must be a node ID string", path)
		}
		for k, v := range x {
			if why := notByID(k); why != "" && v != nil {
				return nil, fmt.Errorf("%s.%s %s", path, k, why)
			}
			if ref, ok := v.(string); ok {
				if err := otherHead(path, k, ref); err != nil {
					return nil, err
				}
			}
			var err error
			if out, err = walkJSON(out, path+"."+k, isIDField(k), v); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	if id {
		return nil, fmt.Errorf("%s must be a node ID string", path)
	}
	return out, nil
}

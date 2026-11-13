package graphql

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// prComment is "read PRs and issues in brig-sh/brig, and comment on them".
var prComment = Rules{
	Queries:   []string{"repository"},
	Mutations: []string{"addComment"},
	Repos:     []string{"brig-sh/brig"},
}

// ghRequest builds the body gh sent for a captured query.
func ghRequest(t *testing.T, base string) []byte {
	t.Helper()
	q, err := os.ReadFile(base + ".graphql")
	if err != nil {
		t.Fatal(err)
	}
	v, err := os.ReadFile(base + ".json")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"query": string(q), "variables": json.RawMessage(v)})
	return body
}

func decide(body []byte, r Rules) string {
	req, err := ReadRequest(body)
	if err == nil {
		_, err = Check(req, r)
	}
	if err != nil {
		return "refused: " + err.Error()
	}
	return "allowed"
}

// TestEveryCapturedGhQueryParses runs the parser on the queries gh 2.103
// sent for pr list/view/checks/status and issue list/view.
func TestEveryCapturedGhQueryParses(t *testing.T) {
	files, _ := filepath.Glob("testdata/gh_*.graphql")
	if len(files) == 0 {
		t.Fatal("no fixtures")
	}
	for _, f := range files {
		src, _ := os.ReadFile(f)
		doc, err := Parse(string(src))
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		op := doc.Operations[0]
		t.Logf("%-40s %s %s root=%v", filepath.Base(f), op.Kind, op.Name, rootFields(doc, op.Selections))
	}
}

func TestCapturedGhQueriesUnderPRCommentPolicy(t *testing.T) {
	want := map[string]string{
		"gh_pr_list_-L_1_1":    "allowed",
		"gh_pr_view_618_1":     "allowed",
		"gh_pr_view_618_2":     "allowed",
		"gh_issue_list_-L_1_1": "allowed",
		"gh_issue_view_268_1":  "allowed",
		"gh_issue_view_268_2":  "allowed",
		"gh_pr_checks_618_1":   "allowed",
		"gh_pr_checks_618_2":   `refused: query field "node" is not in the allow list [repository]`,
		"gh_pr_status_1":       `refused: query field "search" is not in the allow list [repository]`,
	}
	for base, w := range want {
		got := decide(ghRequest(t, "testdata/"+base), prComment)
		t.Logf("%-24s %s", base, got)
		if got != w {
			t.Errorf("%s: got %s, want %s", base, got, w)
		}
	}
}

func TestPRCommentPolicy(t *testing.T) {
	body := func(q string, vars map[string]any) []byte {
		b, _ := json.Marshal(map[string]any{"query": q, "variables": vars})
		return b
	}
	repoVars := map[string]any{"owner": "brig-sh", "repo": "brig"}
	for _, c := range []struct {
		name string
		body []byte
		want string
	}{
		{"gh pr comment", body(`mutation CommentCreate($input: AddCommentInput!){addComment(input: $input){commentEdge{node{url}}}}`,
			map[string]any{"input": map[string]any{"subjectId": "PR_kwDO", "body": "hi"}}), "allowed"},
		{"merge", body(`mutation PullRequestMerge($input: MergePullRequestInput!){mergePullRequest(input: $input){clientMutationId}}`, nil),
			`refused: mutation field "mergePullRequest" is not in the allow list [addComment]`},
		{"comment then merge in one mutation", body(`mutation{a: addComment(input:{}){clientMutationId} b: mergePullRequest(input:{}){clientMutationId}}`, nil),
			`refused: mutation field "mergePullRequest" is not in the allow list [addComment]`},
		{"merge hidden in a fragment", body(`mutation{...m} fragment m on Mutation{mergePullRequest(input:{}){clientMutationId}}`, nil),
			`refused: mutation field "mergePullRequest" is not in the allow list [addComment]`},
		{"aliased to look allowed", body(`mutation{addComment: deleteRepository(input:{}){clientMutationId}}`, nil),
			`refused: mutation field "deleteRepository" is not in the allow list [addComment]`},
		{"other repository", body(`query($owner:String!,$repo:String!){repository(owner:$owner,name:$repo){id}}`,
			map[string]any{"owner": "NOFireAI", "repo": "engineering"}),
			`refused: repo "NOFireAI/engineering" is not in the allow list [brig-sh/brig]`},
		{"case differs", body(`query{repository(owner:"Brig-SH",name:"BRIG"){id}}`, nil), "allowed"},
		{"second repository in an alias", body(`query($owner:String!,$repo:String!){repository(owner:$owner,name:$repo){id} x: repository(owner:"NOFireAI",name:"engineering"){id}}`, repoVars),
			`refused: repo "NOFireAI/engineering" is not in the allow list [brig-sh/brig]`},
		{"root repository in a fragment", body(`query{...f} fragment f on Query{repository(owner:"NOFireAI",name:"engineering"){id}}`, nil),
			`refused: repo "NOFireAI/engineering" is not in the allow list [brig-sh/brig]`},
		// The gap: Repos limits where a query starts, not where it goes.
		{"viewer, which reaches any repository", body(`query{viewer{repository(name:"secret"){issues(first:9){nodes{body}}}}}`, nil),
			`refused: query field "viewer" is not in the allow list [repository]`},
		{"LEAK: an edge leaves the allowed repository", body(`query{repository(owner:"brig-sh",name:"brig"){issue(number:1){parent{repository{issues(first:9){nodes{body}}}}}}}`, nil), "allowed"},
		{"default variable", body(`query($o:String="NOFireAI"){repository(owner:$o,name:"engineering"){id}}`, nil),
			`refused: repo "NOFireAI/engineering" is not in the allow list [brig-sh/brig]`},
		{"undefined variable", body(`query{repository(owner:$o,name:"brig"){id}}`, nil),
			`refused: repository owner: variable $o is not declared in the operation`},
		{"variable not a string", body(`query($o:String){repository(owner:$o,name:"brig"){id}}`, map[string]any{"o": 1}),
			`refused: repository owner: variable $o must be a string`},
		{"block string", body(`query{repository(owner:"""brig-sh""",name:"brig"){id}}`, nil),
			`refused: repository owner: block strings are not supported here; use a quoted string`},
		{"subscription", body(`subscription{x}`, nil), `refused: subscription operations are not supported`},
		{"two operations, none named", body(`query A{viewer{login}} mutation B{mergePullRequest(input:{}){clientMutationId}}`, nil),
			`refused: document has 2 operations; set operationName to choose one`},
		{"undefined fragment", body(`query{...nope}`, nil), `refused: invalid query: undefined fragment "nope"`},
		{"fragment cycle", body(`query{...a} fragment a on Query{...b} fragment b on Query{...a}`, nil),
			`refused: invalid query: fragment "a" spreads itself, directly or through another fragment`},
		{"duplicate query key", []byte(`{"query":"query{viewer{login}}","query":"mutation{mergePullRequest(input:{}){clientMutationId}}"}`),
			`refused: duplicate key "query"`},
		{"batch", []byte(`[{"query":"query{viewer{login}}"}]`), "refused: body must be one JSON object; batches are not supported"},
		{"extra key", []byte(`{"query":"query{viewer{login}}","extensions":{}}`), `refused: unsupported key "extensions"; use only query, operationName and variables`},
		{"not UTF-8", []byte("{\"query\":\"query{viewer{login\xff}}\"}"), `refused: body is not valid UTF-8`},
		{"deep nesting", body("query{"+strings.Repeat("a{", 100)+"b"+strings.Repeat("}", 100)+"}", nil),
			`refused: invalid query: offset 133: document is nested more than 64 levels deep`},
	} {
		got := decide(c.body, prComment)
		t.Logf("%-36s %s", c.name, got)
		if got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
}

// An unterminated body of [ that fits the broker's 8 MiB hold once overflowed
// the stack, which no Go program survives.
func TestDeepJSONIsRefusedNotFatal(t *testing.T) {
	prefix := `{"query":"{viewer{login}}","variables":{"a":`
	b := []byte(prefix + strings.Repeat("[", 8<<20-len(prefix)))
	if _, err := ReadRequest(b); err == nil || !strings.Contains(err.Error(), "nested more than 64 levels deep") {
		t.Fatalf("err = %v, want the nesting limit", err)
	}
}

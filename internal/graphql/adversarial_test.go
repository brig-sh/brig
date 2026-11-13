package graphql

import (
	"encoding/json"
	"testing"
)

// Requests a review built to get past the check.
func TestAdversarial(t *testing.T) {
	body := func(q string, vars map[string]any) []byte {
		b, _ := json.Marshal(map[string]any{"query": q, "variables": vars})
		return b
	}
	for _, c := range []struct {
		name string
		body []byte
		r    Rules
		want string
	}{
		{"duplicate owner, allowed one last", body(`query{repository(owner:"NOFireAI",owner:"brig-sh",name:"brig"){id}}`, nil), prComment,
			`refused: invalid query: offset 50: duplicate argument "owner"`},
		{"duplicate owner, allowed one first", body(`query{repository(owner:"brig-sh",owner:"NOFireAI",name:"brig"){id}}`, nil), prComment,
			`refused: invalid query: offset 50: duplicate argument "owner"`},
		{"variable defined twice", body(`query($o:String="brig-sh",$o:String="NOFireAI"){repository(owner:$o,name:"brig"){id}}`, nil), prComment,
			`refused: invalid query: offset 46: duplicate variable $o`},
		{"input field given twice", body(`mutation{addComment(input:{subjectId:"A",subjectId:"B",body:"x"}){clientMutationId}}`, nil), prComment,
			`refused: invalid query: offset 55: duplicate input field "subjectId"`},
		{"Kelvin sign folds to k", body("query{repository(owner:\"\u212Aelvin\",name:\"x\"){id}}", nil),
			Rules{Queries: []string{"repository"}, Repos: []string{"kelvin/*"}}, `refused: repository owner "` + "\u212A" + `elvin" contains non-ASCII characters`},
		{"variables as a JSON string", []byte(`{"query":"query($o:String!){repository(owner:$o,name:\"brig\"){id}}","variables":"{\"o\":\"NOFireAI\"}"}`), prComment,
			"refused: variables must be a JSON object"},
		{"escaped key duplicate", []byte(`{"query":"{viewer{login}}","\u0071uery":"mutation{mergePullRequest(input:{}){clientMutationId}}"}`), prComment,
			`refused: duplicate key "query"`},
		{"operationName empty with one named op", []byte(`{"query":"mutation M{mergePullRequest(input:{}){clientMutationId}}","operationName":""}`), prComment,
			`refused: mutation field "mergePullRequest" is not in the allow list [addComment]`},
		{"inline fragment at the mutation root", body(`mutation{...on Mutation{mergePullRequest(input:{}){clientMutationId}}}`, nil), prComment,
			`refused: mutation field "mergePullRequest" is not in the allow list [addComment]`},
		{"escape spelling the owner", body(`query{repository(owner:"\u004EOFireAI",name:"engineering"){id}}`, nil), prComment,
			`refused: repo "NOFireAI/engineering" is not in the allow list [brig-sh/brig]`},
		// Known limits: a mutation names a node ID, and an edge leaves the
		// repository a query starts at. The token's scope is the limit.
		{"LIMIT: addComment on another repo's PR", body(`mutation{addComment(input:{subjectId:"PR_of_NOFireAI_engineering",body:"x"}){clientMutationId}}`, nil), prComment, "allowed"},
	} {
		got := decide(c.body, c.r)
		if got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
}

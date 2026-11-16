package graphql

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// A mutation names its target by node ID, not by repository. With repos,
// Check returns every node ID a mutation's arguments carry, so the caller can
// look up the repository of each.
func TestMutationNodeIDsUnderRepos(t *testing.T) {
	rules := Rules{
		Queries:   []string{"repository"},
		Mutations: []string{"addComment", "addLabelsToLabelable", "updateIssue", "createRepository", "followUser", "createCommitOnBranch", "addPullRequestReviewComment", "addSubIssue", "createPullRequest"},
		Repos:     []string{"brig-sh/brig"},
	}
	body := func(q string, vars map[string]any) []byte {
		b, _ := json.Marshal(map[string]any{"query": q, "variables": vars})
		return b
	}
	for _, c := range []struct {
		name string
		body []byte
		want string
	}{
		{"literal subjectId", body(`mutation{addComment(input:{subjectId:"PR_a",body:"hi",clientMutationId:"c1"}){clientMutationId}}`, nil),
			"addComment.input.subjectId=PR_a"},
		{"input in a variable", body(`mutation($i:AddCommentInput!){addComment(input:$i){clientMutationId}}`,
			map[string]any{"i": map[string]any{"subjectId": "I_b", "body": "hi"}}),
			"addComment.input.subjectId=I_b"},
		{"id in a variable", body(`mutation($s:ID!){addComment(input:{subjectId:$s,body:"hi"}){clientMutationId}}`,
			map[string]any{"s": "I_c"}),
			"addComment.input.subjectId=I_c"},
		{"id from a default", body(`mutation($s:ID="I_d"){addComment(input:{subjectId:$s,body:"hi"}){clientMutationId}}`, nil),
			"addComment.input.subjectId=I_d"},
		{"aliases", body(`mutation{a: addComment(input:{subjectId:"PR_a",body:"x"}){clientMutationId} b: addComment(input:{subjectId:"I_e",body:"y"}){clientMutationId}}`, nil),
			"addComment.input.subjectId=PR_a addComment.input.subjectId=I_e"},
		{"a list of ids", body(`mutation{addLabelsToLabelable(input:{labelableId:"I_f",labelIds:["LA_1","LA_2"]}){clientMutationId}}`, nil),
			"addLabelsToLabelable.input.labelableId=I_f addLabelsToLabelable.input.labelIds=LA_1 addLabelsToLabelable.input.labelIds=LA_2"},
		{"a list in a variable", body(`mutation($l:[ID!]!){addLabelsToLabelable(input:{labelableId:"I_f",labelIds:$l}){clientMutationId}}`,
			map[string]any{"l": []any{"LA_3"}}),
			"addLabelsToLabelable.input.labelableId=I_f addLabelsToLabelable.input.labelIds=LA_3"},
		{"a field named id", body(`mutation{updateIssue(input:{id:"I_g",title:"t"}){clientMutationId}}`, nil),
			"updateIssue.input.id=I_g"},
		{"a null id is no id", body(`mutation{updateIssue(input:{id:"I_g",milestoneId:null}){clientMutationId}}`, nil),
			"updateIssue.input.id=I_g"},
		{"an id that is not a string", body(`mutation{addComment(input:{subjectId:7,body:"hi"}){clientMutationId}}`, nil),
			"refused: addComment.input.subjectId must be a node ID string"},
		{"an id object in a variable", body(`mutation($i:AddCommentInput!){addComment(input:$i){clientMutationId}}`,
			map[string]any{"i": map[string]any{"subjectId": map[string]any{"x": "y"}}}),
			"refused: addComment.input.subjectId must be a node ID string"},
		{"no id: createRepository under the viewer", body(`mutation{createRepository(input:{name:"x",visibility:PRIVATE}){clientMutationId}}`, nil),
			`refused: mutation field "createRepository" names no node ID, so repos cannot check it`},
		{"no id beside one that has one", body(`mutation{a: addComment(input:{subjectId:"PR_a",body:"x"}){clientMutationId} b: createRepository(input:{name:"x"}){clientMutationId}}`, nil),
			`refused: mutation field "createRepository" names no node ID, so repos cannot check it`},
		{"a repository named by name beside an allowed ID", body(`mutation{createCommitOnBranch(input:{branch:{repositoryNameWithOwner:"NOFireAI/engineering",branchName:"main"},expectedHeadOid:"abc",message:{headline:"x"},fileChanges:{},clientMutationId:"c"}){clientMutationId} b: addComment(input:{subjectId:"PR_a",body:"x"}){clientMutationId}}`, nil),
			"refused: createCommitOnBranch.input.branch.repositoryNameWithOwner names a repository by name; repos checks node IDs, so use one"},
		{"a repository named by name in a variable", body(`mutation($b:CommittableBranch!){createCommitOnBranch(input:{branch:$b,expectedHeadOid:"abc"}){clientMutationId}}`,
			map[string]any{"b": map[string]any{"repositoryNameWithOwner": "NOFireAI/engineering", "branchName": "main"}}),
			"refused: createCommitOnBranch.input.branch.repositoryNameWithOwner names a repository by name; repos checks node IDs, so use one"},
		{"inReplyTo is a node ID", body(`mutation{addPullRequestReviewComment(input:{pullRequestId:"PR_a",inReplyTo:"PRRC_1",body:"x"}){clientMutationId}}`, nil),
			"addPullRequestReviewComment.input.pullRequestId=PR_a addPullRequestReviewComment.input.inReplyTo=PRRC_1"},
		{"a target named by URL", body(`mutation{addSubIssue(input:{issueId:"I_a",subIssueUrl:"https://github.com/o/r/issues/5"}){clientMutationId}}`, nil),
			"refused: addSubIssue.input.subIssueUrl takes a URL, which repos cannot check; use a node ID field, or leave it out"},
		{"a target named by URL in a variable", body(`mutation($i:AddSubIssueInput!){addSubIssue(input:$i){clientMutationId}}`,
			map[string]any{"i": map[string]any{"issueId": "I_a", "subIssueUrl": "https://github.com/o/r/issues/5"}}),
			"refused: addSubIssue.input.subIssueUrl takes a URL, which repos cannot check; use a node ID field, or leave it out"},
		{"a head branch in the same repository", body(`mutation{createPullRequest(input:{repositoryId:"R_a",baseRefName:"main",headRefName:"feature",title:"t"}){clientMutationId}}`, nil),
			"createPullRequest.input.repositoryId=R_a"},
		{"a head branch in another repository", body(`mutation{createPullRequest(input:{repositoryId:"R_a",baseRefName:"main",headRefName:"fork:feature",title:"t"}){clientMutationId}}`, nil),
			`refused: createPullRequest.input.headRefName "fork:feature" names a branch in another repository; repos checks node IDs, so use headRepositoryId`},
		{"a head branch in another repository, in a variable", body(`mutation($h:String!){createPullRequest(input:{repositoryId:"R_a",baseRefName:"main",headRefName:$h,title:"t"}){clientMutationId}}`,
			map[string]any{"h": "fork:feature"}),
			`refused: createPullRequest.input.headRefName "fork:feature" names a branch in another repository; repos checks node IDs, so use headRepositoryId`},
		{"a head branch in a block string", body(`mutation{createPullRequest(input:{repositoryId:"R_a",baseRefName:"main",headRefName:"""fork:feature""",title:"t"}){clientMutationId}}`, nil),
			"refused: createPullRequest.input.headRefName: block strings are not supported here; use a quoted string"},
		{"a head branch in another repository, in an input variable", body(`mutation($i:CreatePullRequestInput!){createPullRequest(input:$i){clientMutationId}}`,
			map[string]any{"i": map[string]any{"repositoryId": "R_a", "headRefName": "fork:feature"}}),
			`refused: createPullRequest.input.headRefName "fork:feature" names a branch in another repository; repos checks node IDs, so use headRepositoryId`},
		{"an undeclared variable", body(`mutation{addComment(input:{subjectId:$s,body:"hi"}){clientMutationId}}`, nil),
			"refused: addComment.input.subjectId: variable $s is not declared in the operation"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := nodesOf(t, c.body, rules)
			if got != c.want {
				t.Errorf("\n got %s\nwant %s", got, c.want)
			}
		})
	}
}

// Without repos, a mutation needs no lookup.
func TestMutationNodeIDsOnlyUnderRepos(t *testing.T) {
	b, _ := json.Marshal(map[string]any{"query": `mutation{addComment(input:{subjectId:"PR_a",body:"x"}){clientMutationId}}`})
	if got := nodesOf(t, b, Rules{Mutations: []string{"addComment"}}); got != "" {
		t.Errorf("got %q, want no node IDs", got)
	}
}

func nodesOf(t *testing.T, body []byte, r Rules) string {
	t.Helper()
	req, err := ReadRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Check(req, r)
	if err != nil {
		return "refused: " + err.Error()
	}
	var out []string
	for _, n := range res.Nodes {
		out = append(out, fmt.Sprintf("%s=%s", n.Path, n.ID))
	}
	return strings.Join(out, " ")
}

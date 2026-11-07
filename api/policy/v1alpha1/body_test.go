package v1alpha1

import (
	"reflect"
	"strings"
	"testing"
)

// fullBody sets every rule of request.body.
func fullBody() *RequestBody {
	return &RequestBody{
		Models:         []string{"claude-sonnet-4-5*", "claude-opus-4-1"},
		Tools:          &ToolRules{Allow: []string{"Read", "Bash"}, Deny: []string{"WebFetch"}},
		MaxTokens:      8192,
		ToolResultDeny: []string{`AKIA[0-9A-Z]{16}`},
		ToolUse:        &ToolUseRules{Deny: []string{"Bash"}, Action: ToolUseRefuse},
	}
}

// bodyRoute is a claude-code route as a policy writes it, with every body
// rule set.
func bodyRoute(preset Preset) Route {
	return Route{
		Name:       "anthropic",
		Preset:     preset,
		Credential: &CredentialRef{SecretRef: SecretRef{Name: "claude-credentials"}},
		Request:    &RequestPolicy{Body: fullBody()},
	}
}

func TestRequestBodyValidate(t *testing.T) {
	for _, preset := range []Preset{PresetClaudeCode, PresetAnthropicAPIKey} {
		r := bodyRoute(preset)
		if err := r.validateWritten(); err != nil {
			t.Errorf("a full request.body on %s was refused: %v", preset, err)
		}
	}

	// The body rules read the Messages API, so a route that does not speak
	// it is refused, whatever the rest of the route says.
	custom := customRoute()
	custom.Request.Body = fullBody()
	if err := custom.Validate(); err == nil || !strings.Contains(err.Error(),
		"request.body needs preset: claude-code or anthropic-api-key") {
		t.Errorf("request.body on a route with no preset: Validate = %v", err)
	}
	aws := awsRoute()
	aws.Request.Body = fullBody()
	if err := aws.Validate(); err == nil || !strings.Contains(err.Error(),
		"request.body needs preset: claude-code or anthropic-api-key") {
		t.Errorf("request.body on an aws route: Validate = %v", err)
	}

	for _, c := range []struct {
		name string
		edit func(*Route)
		want string
	}{
		{"github-token", func(r *Route) { r.Preset = PresetGitHubToken },
			"request.body needs preset: claude-code or anthropic-api-key"},
		{"empty models", func(r *Route) { r.Request.Body.Models = []string{} },
			"request.body.models is empty; list at least one, or leave the key out"},
		{"empty tools.allow", func(r *Route) { r.Request.Body.Tools.Allow = []string{} },
			"request.body.tools.allow is empty; list at least one, or leave the key out"},
		{"empty tools.deny", func(r *Route) { r.Request.Body.Tools.Deny = []string{} },
			"request.body.tools.deny is empty; list at least one, or leave the key out"},
		{"empty toolResultDeny", func(r *Route) { r.Request.Body.ToolResultDeny = []string{} },
			"request.body.toolResultDeny is empty; list at least one, or leave the key out"},
		{"empty toolUse.deny", func(r *Route) { r.Request.Body.ToolUse.Deny = []string{} },
			"request.body.toolUse.deny is empty; list at least one, or leave the key out"},
		{"body sets nothing", func(r *Route) { r.Request.Body = &RequestBody{} },
			"request.body sets nothing; leave it out to forward every body"},
		{"empty model name", func(r *Route) { r.Request.Body.Models = []string{""} },
			`request.body.models "" is not a model name`},
		{"model name with a space", func(r *Route) { r.Request.Body.Models = []string{"claude sonnet"} },
			`request.body.models "claude sonnet" is not a model name`},
		{"model glob with ?", func(r *Route) { r.Request.Body.Models = []string{"claude-sonnet-4-?"} },
			`request.body.models "claude-sonnet-4-?" holds a glob character other than *`},
		{"model glob with [", func(r *Route) { r.Request.Body.Models = []string{"claude-[so]*"} },
			`request.body.models "claude-[so]*" holds a glob character other than *`},
		{"model glob with a backslash", func(r *Route) { r.Request.Body.Models = []string{`claude-\*`} },
			`request.body.models "claude-\\*" holds a glob character other than *`},
		{"tools with neither list", func(r *Route) { r.Request.Body.Tools = &ToolRules{} },
			"request.body.tools sets neither allow nor deny"},
		{"tool name with a space", func(r *Route) { r.Request.Body.Tools.Allow = []string{"Web Fetch"} },
			`request.body.tools.allow "Web Fetch" is not a tool name`},
		{"empty tool name in deny", func(r *Route) { r.Request.Body.Tools.Deny = []string{""} },
			`request.body.tools.deny "" is not a tool name`},
		{"toolUse.deny name with a space", func(r *Route) { r.Request.Body.ToolUse.Deny = []string{"Ba sh"} },
			`request.body.toolUse.deny "Ba sh" is not a tool name`},
		{"negative maxTokens", func(r *Route) { r.Request.Body.MaxTokens = -1 },
			"request.body.maxTokens -1 is negative"},
		{"pattern RE2 refuses", func(r *Route) { r.Request.Body.ToolResultDeny = []string{"("} },
			`request.body.toolResultDeny "(" is not an RE2 pattern: `},
		{"toolUse without deny", func(r *Route) { r.Request.Body.ToolUse = &ToolUseRules{Action: ToolUseDrop} },
			"request.body.toolUse.deny is required when request.body.toolUse is set"},
		{"unknown action", func(r *Route) { r.Request.Body.ToolUse.Action = "block" },
			`request.body.toolUse.action "block" is not refuse or drop`},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := bodyRoute(PresetClaudeCode)
			c.edit(&r)
			err := r.validateWritten()
			if err == nil {
				t.Fatal("validate accepted it")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not say %q", err, c.want)
			}
			if !strings.HasPrefix(err.Error(), "route anthropic: ") {
				t.Errorf("error %q does not name the route", err)
			}
		})
	}

	// Each rule stands alone, and the default action is refuse.
	for name, body := range map[string]*RequestBody{
		"models":         {Models: []string{"*"}},
		"tools.deny":     {Tools: &ToolRules{Deny: []string{"Bash"}}},
		"maxTokens":      {MaxTokens: 1},
		"toolResultDeny": {ToolResultDeny: []string{"secret"}},
		"toolUse":        {ToolUse: &ToolUseRules{Deny: []string{"Bash"}}},
	} {
		r := bodyRoute(PresetClaudeCode)
		r.Request.Body = body
		if err := r.validateWritten(); err != nil {
			t.Errorf("request.body with %s alone was refused: %v", name, err)
		}
	}
	if got := (&ToolUseRules{Deny: []string{"Bash"}}).Mode(); got != ToolUseRefuse {
		t.Errorf("Mode of an unset action = %q, want %q", got, ToolUseRefuse)
	}
	if got := (&ToolUseRules{Deny: []string{"Bash"}, Action: ToolUseDrop}).Mode(); got != ToolUseDrop {
		t.Errorf("Mode of drop = %q, want %q", got, ToolUseDrop)
	}
}

func TestRequestBodyDecodesStrictly(t *testing.T) {
	head := "apiVersion: brig.sh/v1alpha1\nkind: Policy\nmetadata: {name: claude}\nspec:\n  routes:\n" +
		"    - name: anthropic\n      preset: claude-code\n" +
		"      credential: {secretRef: {name: claude-credentials}}\n" +
		"      request:\n        body:\n"
	doc := head +
		"          models: [claude-sonnet-4-5*, claude-opus-4-1]\n" +
		"          tools: {allow: [Read, Bash], deny: [WebFetch]}\n" +
		"          maxTokens: 8192\n" +
		"          toolResultDeny: ['AKIA[0-9A-Z]{16}']\n" +
		"          toolUse: {deny: [Bash], action: refuse}\n"
	p, err := Decode([]byte(doc))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	req := p.Spec.Routes[0].Request
	if req == nil || req.Body == nil {
		t.Fatalf("request.body was dropped: %+v", req)
	}
	if !reflect.DeepEqual(req.Body, fullBody()) {
		t.Errorf("request.body decoded to %+v, want %+v", req.Body, fullBody())
	}

	for _, c := range []struct{ name, body, want string }{
		{"a key in the wrong case", "          toolresultDeny: [a]\n",
			`unknown field "spec.routes[0].request.body.toolresultDeny"`},
		{"an unknown key", "          temperature: 1\n", `unknown field "temperature"`},
		{"an unknown key under toolUse", "          toolUse: {deny: [Bash], mode: drop}\n", `unknown field "mode"`},
		{"a list with no value", "          models:\n", "has no value"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := Decode([]byte(head + c.body))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("Decode = %v, want %q", err, c.want)
			}
		})
	}
}

// Preset expansion copies a route before it overlays it, so a copy that
// shared a body list would let a run's edit move the policy's rule.
func TestRouteDeepCopyClonesBody(t *testing.T) {
	r := bodyRoute(PresetClaudeCode)
	c := r.DeepCopy()
	if !reflect.DeepEqual(c.Request.Body, r.Request.Body) {
		t.Fatalf("the copy's body %+v differs from %+v", c.Request.Body, r.Request.Body)
	}
	if c.Request.Body == r.Request.Body || c.Request.Body.Tools == r.Request.Body.Tools ||
		c.Request.Body.ToolUse == r.Request.Body.ToolUse {
		t.Fatal("the copy shares a pointer of request.body")
	}
	c.Request.Body.Models[0] = "changed"
	c.Request.Body.Tools.Allow[0] = "changed"
	c.Request.Body.Tools.Deny[0] = "changed"
	c.Request.Body.ToolResultDeny[0] = "changed"
	c.Request.Body.ToolUse.Deny[0] = "changed"
	c.Request.Body.ToolUse.Action = ToolUseDrop
	c.Request.Body.MaxTokens = 1
	if !reflect.DeepEqual(r.Request.Body, fullBody()) {
		t.Errorf("an edit of the copy moved the original's body to %+v", r.Request.Body)
	}

	// An empty list stays empty, so validation of the copy still refuses it.
	r.Request.Body.Models = []string{}
	if err := r.DeepCopy().validateWritten(); err == nil {
		t.Error("DeepCopy turned an empty models list into an unset one")
	}
}

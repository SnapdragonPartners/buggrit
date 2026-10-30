package review

import (
	"context"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"

	"github.com/SnapdragonPartners/buggrit/internal/state"
)

// The identity hashes what each stage really sends: its system prompt, the
// opening's closing instruction, and its tools in order.
func TestIdentityMatchesWhatStagesSend(t *testing.T) {
	id := Identity()
	if len(id) != 2 {
		t.Fatalf("identity stages = %v, want find and verify", id)
	}
	sha := regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	for name, s := range id {
		if !sha.MatchString(s.PromptHash) || !sha.MatchString(s.ToolsHash) {
			t.Errorf("%s identity = %+v, want sha256 hashes", name, s)
		}
	}

	findFake := &testllm.FakeChatClient{Responses: []llms.ChatResponse{
		testllm.ToolCallResponse("1", "submit_findings", `{"coverage": "complete", "findings": []}`),
	}}
	if _, err := Find(context.Background(), findInput(t, findFake)); err != nil {
		t.Fatal(err)
	}
	verifyFake := &testllm.FakeChatClient{Responses: []llms.ChatResponse{
		testllm.ToolCallResponse("1", "submit_verdicts", `{"verdicts": [{"id": "f-aaaaaa", "state": "open", "reason": "Still there."}]}`),
	}}
	check := Check{Tracked: tracked(t, "f-aaaaaa", "P1", state.StateOpen), Why: "it is open"}
	if _, err := Verify(context.Background(), verifyInput(t, verifyFake, check)); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name    string
		prompts stagePrompts
		req     llms.ChatRequest
	}{
		{"find", findPrompts(), findFake.Calls()[0]},
		{"verify", verifyPrompts(), verifyFake.Calls()[0]},
	} {
		if got := c.req.System[0].Text; got != c.prompts.System {
			t.Errorf("%s: system prompt sent isn't the hashed one", c.name)
		}
		if opening := c.req.Messages[0].Content[0].Text; !strings.Contains(opening, c.prompts.Closing) {
			t.Errorf("%s: opening doesn't carry the hashed closing instruction", c.name)
		}
		if got := contentHash(c.req.Tools); got != id[c.name].ToolsHash {
			t.Errorf("%s: tools sent hash to %s, identity says %s", c.name, got, id[c.name].ToolsHash)
		}
	}
}

// Every field of a stage's prompts is set and moves its hash, so a field
// added later can't be left out of the identity.
func TestPromptHashCoversEveryField(t *testing.T) {
	for name, p := range map[string]stagePrompts{"find": findPrompts(), "verify": verifyPrompts()} {
		base := contentHash(p)
		v := reflect.ValueOf(p)
		for i := range v.NumField() {
			field := v.Type().Field(i).Name
			if v.Field(i).String() == "" {
				t.Errorf("%s prompts: %s is empty", name, field)
			}
			changed := p
			reflect.ValueOf(&changed).Elem().Field(i).SetString(v.Field(i).String() + " ")
			if contentHash(changed) == base {
				t.Errorf("%s prompts: changing %s doesn't change the hash", name, field)
			}
		}
	}
}

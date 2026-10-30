package review

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"
)

// stagePrompts is a stage's standing instructions: the fixed text that tells
// the model how to work. The opening message's other text (its headings and
// how it packs the pass's input) is harness, identified by the commit. Every
// field is hashed, so a new one joins the stage's prompt identity.
type stagePrompts struct {
	System string `json:"system"`
	// Closing ends the opening message.
	Closing string `json:"closing"`
	Nudge   string `json:"nudge"`
	TimeUp  string `json:"time_up"`
}

func findPrompts() stagePrompts {
	return stagePrompts{System: systemPrompt, Closing: findClosing, Nudge: nudge, TimeUp: timeUp}
}

func verifyPrompts() stagePrompts {
	return stagePrompts{System: verifySystemPrompt, Closing: verifyClosing, Nudge: verifyNudge, TimeUp: verifyTimeUp}
}

// findTools and verifyTools are each stage's tools in the order offered,
// the terminal tool last. A nil environment builds the definitions alone.
func findTools(env *toolEnv, submit *submitTool) (tools []toolloop.Tool, terminal toolloop.Tool) {
	return readOnlyTools(env), submit.tool()
}

func verifyTools(env *toolEnv, threads *threadTool, submit *verdictTool) (tools []toolloop.Tool, terminal toolloop.Tool) {
	return append(readOnlyTools(env), threads.tool()), submit.tool()
}

// StageIdentity is the prompt and tools part of a stage's model/prompt/
// harness identity (design §6, "Telemetry"): content hashes, so runs group
// by what the model was told and offered, whatever commit built them.
type StageIdentity struct {
	PromptHash string `json:"prompt_hash"`
	ToolsHash  string `json:"tools_hash"`
}

// Identity is each agentic stage's identity, by stage name.
func Identity() map[string]StageIdentity {
	find, findTerminal := findTools(nil, &submitTool{})
	verify, verifyTerminal := verifyTools(nil, &threadTool{}, &verdictTool{})
	return map[string]StageIdentity{
		"find":   {PromptHash: contentHash(findPrompts()), ToolsHash: contentHash(toolDefs(find, findTerminal))},
		"verify": {PromptHash: contentHash(verifyPrompts()), ToolsHash: contentHash(toolDefs(verify, verifyTerminal))},
	}
}

func toolDefs(tools []toolloop.Tool, terminal toolloop.Tool) []llms.ToolDefinition {
	defs := make([]llms.ToolDefinition, 0, len(tools)+1)
	for i := range tools {
		defs = append(defs, tools[i].Definition)
	}
	return append(defs, terminal.Definition)
}

// contentHash is the "sha256:" hash of v's JSON, which is canonical for the
// shapes hashed here: struct fields in order, and raw schemas compacted.
func contentHash(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // only fixed, marshalable shapes are hashed
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

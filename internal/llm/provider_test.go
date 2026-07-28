package llm

import (
	"encoding/json"
	"testing"
)

func TestMessageJSONRoundTrip(t *testing.T) {
	msg := AssistantMessage(
		TextBlock("checking"),
		ToolUseBlock(ToolUse{ID: "toolu_01", Name: "query_range", Input: json.RawMessage(`{"query":"up"}`)}),
	)
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Message
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Role != RoleAssistant || len(got.Content) != 2 {
		t.Fatalf("round trip lost structure: %+v", got)
	}
	if got.Content[0].Type != BlockText || got.Content[0].Text != "checking" {
		t.Errorf("text block = %+v", got.Content[0])
	}
	tu := got.Content[1]
	if tu.Type != BlockToolUse || tu.ToolUse == nil || tu.ToolUse.ID != "toolu_01" {
		t.Errorf("tool_use block = %+v", tu)
	}
	if string(tu.ToolUse.Input) != `{"query":"up"}` {
		t.Errorf("tool input = %s", tu.ToolUse.Input)
	}
}

func TestMessageHelpers(t *testing.T) {
	msg := Message{Role: RoleAssistant, Content: []ContentBlock{
		TextBlock("a"),
		ToolUseBlock(ToolUse{ID: "1", Name: "x"}),
		TextBlock("b"),
	}}
	if got := msg.Text(); got != "ab" {
		t.Errorf("Text() = %q, want ab", got)
	}
	if uses := msg.ToolUses(); len(uses) != 1 || uses[0].ID != "1" {
		t.Errorf("ToolUses() = %+v", uses)
	}
}

func TestToolChoiceOmittedWhenZero(t *testing.T) {
	data, err := json.Marshal(Request{Model: "m", MaxTokens: 1})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["tool_choice"]; ok {
		t.Errorf("zero ToolChoice serialized: %s", data)
	}
}

func TestChooseTool(t *testing.T) {
	tc := ChooseTool("submit_finding")
	if tc.Type != ToolChoiceTool || tc.Name != "submit_finding" {
		t.Errorf("ChooseTool = %+v", tc)
	}
}

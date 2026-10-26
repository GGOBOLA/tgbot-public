package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A stream may interleave blocks: deltas name the block they belong to, so the
// assembler must attach each one to that block and not to whatever arrived last.
func TestClaudeAssemblerKeysDeltasByBlockIndex(t *testing.T) {
	stream := []string{
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"calc","input":{}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"a"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"b"}}`,
	}

	var a claudeAssembler
	for _, raw := range stream {
		var ev claudeEvent
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			t.Fatalf("unmarshal %s: %v", raw, err)
		}
		a.apply(ev)
	}

	var got struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(a.render(), &got); err != nil {
		t.Fatalf("rendered message is not valid JSON: %v", err)
	}
	if len(got.Content) != 2 {
		t.Fatalf("got %d blocks, want 2: %s", len(got.Content), a.render())
	}

	if got.Content[0].Type != "text" {
		t.Fatalf("block 0 type = %q, want text", got.Content[0].Type)
	}
	if got.Content[0].Text != "ab" {
		t.Fatalf("block 0 text = %q, want %q", got.Content[0].Text, "ab")
	}

	if got.Content[1].Type != "tool_use" {
		t.Fatalf("block 1 type = %q, want tool_use", got.Content[1].Type)
	}
	if got.Content[1].Name != "calc" {
		t.Fatalf("block 1 name = %q, want calc", got.Content[1].Name)
	}
	if string(got.Content[1].Input) != "{}" {
		t.Fatalf("block 1 input = %s, want {}", got.Content[1].Input)
	}
}

// The block index comes from the upstream. An absurd one must not size the
// slice: before the cap, one event with index 1e8 allocated gigabytes and the
// OOM killer took the bot and the gateway with it.
func TestSetBlockRefusesAbsurdIndex(t *testing.T) {
	var a claudeAssembler
	a.setBlock(1<<20, claudeBlock{Type: "text", Text: "x"})
	if len(a.blocks) != 0 {
		t.Fatalf("an absurd index allocated %d blocks", len(a.blocks))
	}
	a.setBlock(maxClaudeBlocks, claudeBlock{Type: "text", Text: "x"})
	if len(a.blocks) != 0 {
		t.Fatalf("an index at the cap allocated %d blocks", len(a.blocks))
	}

	a.setBlock(0, claudeBlock{Type: "text", Text: "hi"})
	if len(a.blocks) != 1 || a.blocks[0].Text != "hi" {
		t.Fatalf("a normal index was refused: %+v", a.blocks)
	}
}

// An event with no index is not index 0: a delta belongs to the block being
// streamed, which is the last one started.
func TestClaudeDeltaWithoutIndexExtendsLastBlock(t *testing.T) {
	stream := []string{
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":"second"}}`,
	}
	var a claudeAssembler
	for _, raw := range stream {
		var ev claudeEvent
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			t.Fatalf("unmarshal %s: %v", raw, err)
		}
		a.apply(ev)
	}
	if a.blocks[0].Text != "" || a.blocks[1].Text != "second" {
		t.Fatalf("blocks = %q / %q, want the text on the last block", a.blocks[0].Text, a.blocks[1].Text)
	}
}

// Anthropic requires a text field on a text block, and forbids one on tool_use.
func TestClaudeRenderEmitsTextOnlyWhereItBelongs(t *testing.T) {
	var a claudeAssembler
	for _, raw := range []string{
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"t1","name":"calc","input":{}}}`,
	} {
		var ev claudeEvent
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			t.Fatal(err)
		}
		a.apply(ev)
	}
	var got struct {
		Content []map[string]json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(a.render(), &got); err != nil {
		t.Fatalf("render: %v", err)
	}
	if _, ok := got.Content[0]["text"]; !ok {
		t.Fatalf("text block rendered without a text field: %v", got.Content[0])
	}
	if _, ok := got.Content[1]["text"]; ok {
		t.Fatalf("tool_use block carries a text field: %v", got.Content[1])
	}
	if _, ok := got.Content[1]["input"]; !ok {
		t.Fatalf("tool_use block lost its input: %v", got.Content[1])
	}
}

// A non-streaming Claude call is answered from the SSE stream, so a stream that
// stops before message_stop must be an error: rendering the prefix as a complete
// message told the client a truncated answer was finished.
func TestClaudeStreamWithoutStopIsRejected(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_1","model":"m","role":"assistant","usage":{"input_tokens":10}}}`,
		``,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"half an ans"}}`,
		``,
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	rec := httptest.NewRecorder()
	used, delivered := (&Server{}).writeClaudeJSON(rec, resp)
	if delivered {
		t.Fatal("a stream with no message_stop was reported as delivered")
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if used.PromptTokens != 0 || used.CompletionTokens != 0 {
		t.Fatalf("usage = %+v, want zero on a rejected stream", used)
	}
}

// The same stream with a stop event is a normal answer.
func TestClaudeStreamWithStopIsAccepted(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_1","model":"m","role":"assistant","usage":{"input_tokens":10}}}`,
		``,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}}`,
		``,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
		``,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	rec := httptest.NewRecorder()
	used, delivered := (&Server{}).writeClaudeJSON(rec, resp)
	if !delivered || rec.Code != http.StatusOK {
		t.Fatalf("delivered=%v status=%d, want a normal answer", delivered, rec.Code)
	}
	if used.PromptTokens != 10 || used.CompletionTokens != 3 {
		t.Fatalf("usage = %+v, want 10/3", used)
	}
	if !strings.Contains(rec.Body.String(), `"text":"done"`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

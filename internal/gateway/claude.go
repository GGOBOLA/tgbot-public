package gateway

import (
	"bufio"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// Anthropic Messages SSE event types, and the fields the assembler needs.
type claudeEvent struct {
	Type    string `json:"type"`
	Message *struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Role  string `json:"role"`
		Usage *claudeUsage
	} `json:"message"`
	// Index is a pointer because an absent index is not index 0: a plain int
	// attached a delta with no index to the first block instead of the last.
	Index        *int `json:"index"`
	ContentBlock *struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *claudeUsage `json:"usage"`
}

// claudeUsage is the Anthropic token accounting, named differently from OpenAI's.
// InputTokens EXCLUDES both cache fields: the prompt total is the sum of all
// three. Cache writes cost 1.25x base input for the 5-minute TTL and 2x for the
// 1-hour TTL; cache reads cost 0.1x.
type claudeUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	// CacheCreation splits the writes by TTL when the API reports it.
	CacheCreation *struct {
		Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
		Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

// writes1h returns the share of cache writes on the 1-hour TTL.
func (c claudeUsage) writes1h() int64 {
	if c.CacheCreation == nil {
		return 0
	}
	if c.CacheCreation.Ephemeral1h > c.CacheCreationInputTokens {
		return c.CacheCreationInputTokens
	}
	return c.CacheCreation.Ephemeral1h
}

// merge folds next into u without letting a later event erase an earlier count.
// Anthropic splits the numbers: message_start carries input tokens, message_delta
// carries output tokens only, so overwriting would zero the input side.
func (u *claudeUsage) merge(next claudeUsage) {
	if next.InputTokens > 0 {
		u.InputTokens = next.InputTokens
	}
	if next.OutputTokens > 0 {
		u.OutputTokens = next.OutputTokens
	}
	if next.CacheCreationInputTokens > 0 {
		u.CacheCreationInputTokens = next.CacheCreationInputTokens
	}
	if next.CacheReadInputTokens > 0 {
		u.CacheReadInputTokens = next.CacheReadInputTokens
	}
	if next.CacheCreation != nil {
		u.CacheCreation = next.CacheCreation
	}
}

// toUsage maps Anthropic accounting onto the gateway's billing shape.
func (c claudeUsage) toUsage() usage {
	return usage{
		PromptTokens:          c.InputTokens,
		CompletionTokens:      c.OutputTokens,
		CacheReadTokens:       c.CacheReadInputTokens,
		CacheCreationTokens:   c.CacheCreationInputTokens,
		CacheCreation1hTokens: c.writes1h(),
	}
}

// claudeUsageEnvelope reads usage out of a JSON Messages response.
type claudeUsageEnvelope struct {
	Usage claudeUsage `json:"usage"`
}

// claudeAssembler rebuilds one Anthropic message from its SSE events.
// maxClaudeBlocks bounds how many blocks one message may declare. Anthropic
// messages carry a handful; the cap exists so an upstream index cannot size the
// slice.
const maxClaudeBlocks = 1024

type claudeAssembler struct {
	id         string
	model      string
	role       string
	blocks     []claudeBlock
	stopReason string
	usage      claudeUsage
	// complete records that the stream said it was finished (message_stop, or a
	// message_delta carrying a stop_reason). Without it the assembled message is
	// a prefix of a real answer, and rendering it as "end_turn" presented a
	// truncated response as complete.
	complete bool
}

// blockIndex resolves an event's index, falling back when the upstream omitted
// it. A start with no index appends; a delta with no index extends the last
// block, which is the one still being streamed.
func (e claudeEvent) blockIndex(fallback int) int {
	if e.Index == nil {
		return fallback
	}
	return *e.Index
}

type claudeBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// partial accumulates input_json_delta fragments for tool_use blocks; it is
	// folded into Input at render time and never serialized.
	partial string
}

// apply folds one parsed event into the message under construction.
func (a *claudeAssembler) apply(ev claudeEvent) {
	switch ev.Type {
	case "message_start":
		if ev.Message != nil {
			a.id, a.model, a.role = ev.Message.ID, ev.Message.Model, ev.Message.Role
			if ev.Message.Usage != nil {
				a.usage.merge(*ev.Message.Usage)
			}
		}
	case "content_block_start":
		// Text blocks are created empty here and filled by deltas; tool_use
		// blocks keep their id/name so the caller can actually invoke the tool.
		// The block belongs at the index the event names, so an out-of-order
		// start lands in its own slot instead of shifting the others.
		blk := claudeBlock{Type: "text"}
		if ev.ContentBlock != nil {
			blk = claudeBlock{
				Type:  ev.ContentBlock.Type,
				Text:  ev.ContentBlock.Text,
				ID:    ev.ContentBlock.ID,
				Name:  ev.ContentBlock.Name,
				Input: ev.ContentBlock.Input,
			}
		}
		a.setBlock(ev.blockIndex(len(a.blocks)), blk)
	case "content_block_delta":
		// Deltas carry the index of the block they extend: appending to the
		// last block attaches interleaved text or tool JSON to the wrong one.
		idx := ev.blockIndex(len(a.blocks) - 1)
		if ev.Delta == nil || idx < 0 || idx >= len(a.blocks) {
			break
		}
		blk := &a.blocks[idx]
		switch {
		case ev.Delta.Type == "input_json_delta" || ev.Delta.PartialJSON != "":
			// Tool arguments arrive as JSON fragments that must be concatenated.
			blk.partial += ev.Delta.PartialJSON
		case ev.Delta.Text != "":
			blk.Text += ev.Delta.Text
		}
	case "message_delta":
		if ev.Delta != nil && ev.Delta.StopReason != "" {
			a.stopReason = ev.Delta.StopReason
			a.complete = true
		}
		// message_delta reports the final output count; input came from
		// message_start, so merge rather than replace.
		if ev.Usage != nil {
			a.usage.merge(*ev.Usage)
		}
	case "message_stop":
		a.complete = true
	}
}

// setBlock stores blk at idx, growing the slice when the stream has not named
// that index yet. Growth only pads the gap; the block keeps its own position so
// render can emit blocks in index order. Indices at or above maxClaudeBlocks are
// refused: they would otherwise size an allocation from an upstream number.
func (a *claudeAssembler) setBlock(idx int, blk claudeBlock) {
	if idx < 0 {
		// A malformed start with no usable index: append rather than drop it.
		idx = len(a.blocks)
	}
	if idx >= maxClaudeBlocks {
		// The index comes from the upstream, so an absurd value would let it
		// size this slice: one event with a huge index used to allocate until
		// the process was OOM-killed. A stream cannot legitimately have this
		// many blocks, so the event is dropped.
		return
	}
	for len(a.blocks) <= idx {
		a.blocks = append(a.blocks, claudeBlock{})
	}
	a.blocks[idx] = blk
}

// render produces the non-streaming JSON body an Anthropic client expects.
func (a *claudeAssembler) render() []byte {
	contents := make([]any, 0, len(a.blocks))
	for _, b := range a.blocks {
		if b.Type == "" {
			// An index the stream started no block at; emitting it would put an
			// empty unnamed block in the middle of the content array.
			continue
		}
		if b.Type == "tool_use" {
			if b.partial != "" {
				// Fold the accumulated fragments in; invalid JSON would make the
				// whole message unparsable, so fall back to an empty object.
				raw := json.RawMessage(b.partial)
				if json.Valid(raw) {
					b.Input = raw
				} else {
					b.Input = json.RawMessage("{}")
				}
			}
			if len(b.Input) == 0 {
				b.Input = json.RawMessage("{}")
			}
			contents = append(contents, map[string]any{
				"type": "tool_use", "id": b.ID, "name": b.Name, "input": b.Input,
			})
			continue
		}
		// Anthropic requires text on a text block: omitempty rendered an empty
		// one as {"type":"text"}, which a strict client rejects.
		contents = append(contents, map[string]any{"type": b.Type, "text": b.Text})
	}
	usage := map[string]int64{
		"input_tokens":  a.usage.InputTokens,
		"output_tokens": a.usage.OutputTokens,
	}
	// Mirror the cache fields back when the upstream reported them: Anthropic
	// clients read them, and dropping them makes a cached call look uncached.
	if a.usage.CacheCreationInputTokens > 0 {
		usage["cache_creation_input_tokens"] = a.usage.CacheCreationInputTokens
	}
	if a.usage.CacheReadInputTokens > 0 {
		usage["cache_read_input_tokens"] = a.usage.CacheReadInputTokens
	}
	body := map[string]any{
		"id":            firstNonEmpty(a.id, "msg_gateway"),
		"type":          "message",
		"role":          firstNonEmpty(a.role, "assistant"),
		"model":         a.model,
		"content":       contents,
		"stop_reason":   firstNonEmpty(a.stopReason, "end_turn"),
		"stop_sequence": nil,
		"usage":         usage,
	}
	out, err := json.Marshal(body)
	if err != nil {
		return []byte(`{"type":"message","role":"assistant","content":[]}`)
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// countingReader reports how many bytes the scanner consumed, so hitting the
// relay cap can be told apart from the upstream finishing.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// writeClaudeJSON converts an upstream SSE stream into a single JSON message. This
// is required: 9router streams even for a non-streaming request, and a Claude SDK
// pointed at it would choke on raw events. delivered is false when the assembled
// message never reached the client, which is not a billable call.
func (s *Server) writeClaudeJSON(w http.ResponseWriter, resp *http.Response) (usage, bool) {
	defer func() { _ = resp.Body.Close() }()

	var a claudeAssembler
	body := &countingReader{r: io.LimitReader(resp.Body, maxRelayBytes+1)}
	scanner := bufio.NewScanner(body)
	// A single data line can be a whole tool_use input; 4 MB was less than the
	// request cap, so a legal call could fail to parse after the provider billed it.
	scanner.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if !strings.HasPrefix(strings.TrimSpace(string(line)), "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(line)), "data:"))
		if payload == "" {
			continue
		}
		var ev claudeEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue
		}
		a.apply(ev)
	}
	if err := scanner.Err(); err != nil {
		log.Printf("gateway: claude stream read: %v", err)
		writeErr(w, http.StatusBadGateway, "upstream stream broke")
		return usage{}, false
	}
	if body.n > maxRelayBytes {
		// LimitReader at exactly the cap ends in io.EOF, which used to look like
		// a clean end: an over-long stream was delivered as complete and billed
		// only for the events read.
		log.Printf("gateway: claude stream exceeded %d bytes", int64(maxRelayBytes))
		writeErr(w, http.StatusBadGateway, "upstream response too large")
		return usage{}, false
	}
	if !a.complete {
		// No message_stop and no stop_reason: the answer is a prefix. Rendering
		// it with "end_turn" told the client a truncated reply was complete, and
		// billed only the tokens that happened to arrive.
		log.Printf("gateway: claude stream ended without message_stop")
		writeErr(w, http.StatusBadGateway, "upstream stream ended early")
		return usage{}, false
	}

	status := resp.StatusCode
	if status == 0 {
		status = http.StatusOK
	}
	if status >= 300 {
		// The upstream failed. Returning 200 with an empty message would hide the
		// error from the client and bill nothing.
		writeErr(w, status, "upstream error")
		return usage{}, true
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(a.render())
	return a.usage.toUsage(), true
}

// relayClaudeStream passes the Anthropic SSE through untouched for streaming
// callers, while remembering the usage it reports. The second return is the
// wait for the first relayed byte in milliseconds.
func relayClaudeStream(w http.ResponseWriter, resp *http.Response, start time.Time) (usage, int64, bool) {
	defer func() { _ = resp.Body.Close() }()
	sseHeaders(w)
	var seen claudeUsage
	ttft, received, err := relaySSE(w, resp.Body, start, func(line []byte) {
		if u, ok := claudeUsageFromLine(line); ok {
			seen.merge(u)
		}
	})
	if err != nil {
		log.Printf("gateway: claude stream copy: %v", err)
	}
	return seen.toUsage(), ttft, received > 0
}

// claudeUsageFromLine pulls usage out of one SSE "data:" line, if it carries any.
func claudeUsageFromLine(line []byte) (claudeUsage, bool) {
	trimmed := strings.TrimSpace(string(line))
	if !strings.HasPrefix(trimmed, "data:") {
		return claudeUsage{}, false
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	var ev claudeEvent
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		return claudeUsage{}, false
	}
	if ev.Type == "message_delta" && ev.Usage != nil {
		return *ev.Usage, true
	}
	if ev.Type == "message_start" && ev.Message != nil && ev.Message.Usage != nil {
		return *ev.Message.Usage, true
	}
	return claudeUsage{}, false
}

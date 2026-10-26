package gateway

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestEstimatePromptTokens(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int64
	}{
		{"ascii four chars per token", "abcdefgh", 2},
		{"each wide rune is a token", "你好世界", 4},
		// "hi " is three ASCII runes (under one token), plus two wide runes.
		{"mixed", "hi 你好", 2},
		{"empty", "", 0},
	}
	for _, c := range cases {
		if got := estimatePromptTokens([]byte(c.body)); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

// TestBilledPromptTokens locks the pad correction in both directions: it must
// strip a real 9router pad, and it must leave an honest count alone.
func TestBilledPromptTokens(t *testing.T) {
	cases := []struct {
		name     string
		reported int64
		estimate int64
		want     int64
		wantPad  bool
	}{
		// The measured defect: a one-word prompt reported as 2006.
		{"padded one-word prompt", 2006, 27, 6, true},
		// Same pad on a 800-word prompt: 805 real + 2000.
		{"padded large prompt", 2805, 1030, 805, true},
		// A genuinely 2500-token prompt streamed through 9router: 9router only
		// pads converted responses, but even if we saw 4500 the subtraction lands
		// on the estimate.
		{"padded genuine long prompt", 4500, 2500, 2500, true},
		// Honest counts stay untouched.
		{"small honest count", 6, 20, 6, false},
		{"large honest count", 2500, 2500, 2500, false},
		{"enormous honest count", 998000, 20, 998000, false},
		{"honest count far above estimate", 8000, 8000, 8000, false},
		{"subtraction moves farther from estimate", 5000, 8000, 5000, false},
		// 9router fixed: a real 2000-token prompt now reports 2000. Subtracting
		// the pad would zero it, and the estimate rules that out.
		{"honest count at the pad boundary", 2000, 900, 2000, false},
	}
	for _, c := range cases {
		got, padded := billedPromptTokens(c.reported, c.estimate)
		if got != c.want || padded != c.wantPad {
			t.Errorf("%s: got (%d, %v), want (%d, %v)", c.name, got, padded, c.want, c.wantPad)
		}
	}
}

// TestStreamedCallBillsUnpaddedPrompt is the end-to-end lock: a streamed call
// whose upstream claims 2006 prompt tokens for a two-word prompt must be
// charged for 6, both in the ledger and in the stored usage row.
func TestStreamedCallBillsUnpaddedPrompt(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for _, c := range []string{
			"data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n",
			"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2006,\"completion_tokens\":4}}\n\n",
			"data: [DONE]\n\n",
		} {
			_, _ = io.WriteString(w, c)
			flusher.Flush()
		}
	})

	rec := h.do(t, `{"model":"test/model","stream":true,"messages":[{"role":"user","content":"hi there"}]}`, testKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d: %s", rec.Code, rec.Body.String())
	}

	// 6 prompt @ RM2/1M = 1200 µsen + 4 completion @ RM6/1M = 2400 µsen.
	const wantCost = 3_600
	cust, err := h.store.Account(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if got := int64(cust.Balance); got != 1000*usenPerSen-wantCost {
		t.Fatalf("balance %d, want %d (pad billed)", got, 1000*usenPerSen-wantCost)
	}
	_, prompt, completion, spend, err := h.store.UsageTotals(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if prompt != 6 || completion != 4 || int64(spend) != wantCost {
		t.Fatalf("usage prompt=%d completion=%d spend=%d, want 6/4/%d", prompt, completion, spend, wantCost)
	}
}

func TestNonStreamedCallBillsFullLargePromptCount(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":8000,"completion_tokens":0}}`)
	})

	body := `{"model":"test/model","stream":false,"messages":[{"role":"user","content":"` + strings.Repeat("a", 20000) + `"}]}`
	if estimate := int64(len(body) / 4); estimate <= 4000 {
		t.Fatalf("test prompt estimate %d, want > 4000", estimate)
	}
	if rec := h.do(t, body, testKey); rec.Code != http.StatusOK {
		t.Fatalf("code %d: %s", rec.Code, rec.Body.String())
	}
	_, prompt, _, _, err := h.store.UsageTotals(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if prompt != 8000 {
		t.Fatalf("billed prompt tokens %d, want full reported count 8000", prompt)
	}
}

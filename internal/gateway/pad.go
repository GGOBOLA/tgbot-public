package gateway

import "unicode/utf8"

// proxyPromptPad is the number of prompt tokens 9router adds to a response's
// usage whenever it converts that response: every streamed completion, and every
// /v1/messages call (that adapter always translates). Measured on 2026-10-25
// against 9router 0.5.109: an identical request reported prompt_tokens 6
// without streaming and 2006 with it, an 800-word prompt reported 805 and 2805,
// RiverAI's own API reported the same 6 in both modes, and the model confirmed
// it received no extra instructions. So the pad is accounting only — no tokens
// are sent — and billing it charges the customer for tokens nobody spent.
//
// It lives in 9router's bundle (chunks/5330.js, module 73483) as
// `b.prompt_tokens += 2e3`, so the only place it can be undone is here.
const proxyPromptPad = 2000

// estimatePromptTokens approximates how many prompt tokens a request body is
// worth without shipping a provider tokenizer: ASCII at the usual four
// characters per token, every non-ASCII rune as one token (CJK and emoji
// tokenize at roughly one per character). It decides only whether a reported
// count contains the pad; it is never the number we bill.
func estimatePromptTokens(body []byte) int64 {
	var ascii, wide int64
	for _, r := range string(body) {
		if r < utf8.RuneSelf {
			ascii++
			continue
		}
		wide++
	}
	return ascii/4 + wide
}

// billedPromptTokens strips the proxy pad only when what remains is a better
// match for this request's local estimate, using a 64-token floor for small
// estimates so the JSON envelope never masks a padded one-word prompt.
//
// The guards are what keep the correction honest in both directions. A
// genuinely large prompt is left alone, because subtracting the pad would land
// nowhere near the estimate. A 9router that stops padding is left alone too: its
// true small count minus 2000 is negative, so nothing is subtracted. Only the
// pad's actual signature — reported count, minus 2000, sitting on the estimate —
// triggers a correction.
func billedPromptTokens(reported, estimate int64) (billed int64, padded bool) {
	if reported < proxyPromptPad {
		return reported, false
	}
	unpadded := reported - proxyPromptPad
	if absTokenDiff(unpadded, estimate) >= absTokenDiff(reported, estimate) {
		return reported, false
	}
	tolerance := estimate / 2
	if tolerance < 64 {
		tolerance = 64
	}
	if diff := unpadded - estimate; diff > tolerance || -diff > tolerance {
		return reported, false
	}
	return unpadded, true
}

func absTokenDiff(a, b int64) int64 {
	d := a - b
	if d < 0 {
		return -d
	}
	return d
}

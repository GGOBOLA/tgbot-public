package screens

import "hash/fnv"

// ModelArg turns a model id into the int64 a callback payload can carry. Model
// ids contain '/' and '.', which the callback alphabet forbids, so a button
// carries this hash and the handler resolves it against the live catalogue.
// FNV-1a over the id is stable across restarts, and a collision across a
// catalogue of this size is not a practical concern; the bot refuses an
// ambiguous hash instead of guessing. The value is masked to 48 bits so the
// signed payload keeps comfortable room inside Telegram's 64-byte limit.
func ModelArg(id string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(id))
	// 48 bits, not 63: a 19-digit argument plus the action and owner left one
	// byte of the 64-byte callback allowance, so a slightly longer action name
	// made the whole price screen fail to render. 2^48 is still far more than a
	// catalogue needs.
	u := h.Sum64() & (1<<48 - 1)
	// #nosec G115 -- the mask clears the sign bit, so u always fits an int64.
	return int64(u)
}

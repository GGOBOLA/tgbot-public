// Package screens renders dashboard screens as pure view-models: text plus
// button intents. It imports no Telegram types, so a screen can be tested by
// comparing strings and button actions.
package screens

import "tgbot/internal/authz"

// Button is one tappable intent. Arg carries an optional id — a key id, a page
// number, an amount — and never a rendered value the client could edit.
type Button struct {
	Text   string
	Action authz.Action
	Owner  int64
	Arg    int64
}

// Row is one line of buttons.
type Row []Button

// View is a rendered screen.
type View struct {
	Text string
	Rows []Row
	// Plain marks a screen whose text contains data this program does not
	// control (model ids from the upstream, customer-supplied names). It is
	// sent without a parse mode: Markdown in Telegram v1 has no escape syntax,
	// so a stray '_' or '*' would either render wrong or make Telegram reject
	// the whole message with "can't parse entities".
	Plain bool
}

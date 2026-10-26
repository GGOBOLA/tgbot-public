// Package authz is the single authorization gate. Every bot action resolves an
// Action and asks Can; handlers never check ADMIN_IDS themselves.
package authz

// Action names one thing a user can do.
type Action string

const (
	// ActionViewBalance shows the caller's wallet.
	ActionViewBalance Action = "view_balance"
	// ActionViewUsage shows the caller's usage.
	ActionViewUsage Action = "view_usage"
	// ActionGenerateKey mints an API key.
	ActionGenerateKey Action = "generate_key"
	// ActionRevokeKey revokes one of the caller's keys.
	ActionRevokeKey Action = "revoke_key"
	// ActionClaimTopUp opens a payment claim.
	ActionClaimTopUp Action = "claim_topup"
	// ActionMenu returns to the dashboard menu.
	ActionMenu Action = "menu"
	// ActionListKeys lists the caller's keys.
	ActionListKeys Action = "list_keys"
	// ActionViewPrices shows the price list.
	ActionViewPrices Action = "view_prices"
	// ActionConfirmClaim opens a top-up claim for the chosen amount.
	ActionConfirmClaim Action = "confirm_claim"
	// ActionAdminApproveClaim credits a pending claim.
	ActionAdminApproveClaim Action = "admin_approve_claim"
	// ActionAdminRejectClaim rejects a pending claim.
	ActionAdminRejectClaim Action = "admin_reject_claim"
	// ActionAdminViewCustomer opens one customer's card.
	ActionAdminViewCustomer Action = "admin_view_customer"
	// ActionAdminToday shows the platform summary.
	ActionAdminToday Action = "admin_today"
	// ActionAdminListUsers lists every customer.
	ActionAdminListUsers Action = "admin_list_users"
	// ActionAdminCredit credits a customer balance.
	ActionAdminCredit Action = "admin_credit"
	// ActionAdminPrices opens the editable price list.
	ActionAdminPrices Action = "admin_prices"
	// ActionAdminPricesPriced opens the priced model list.
	ActionAdminPricesPriced Action = "admin_prices_priced"
	// ActionAdminPricesUnpriced opens the unpriced model list.
	ActionAdminPricesUnpriced Action = "admin_prices_unpriced"
	// ActionAdminPriceModel opens one model's price editor.
	ActionAdminPriceModel Action = "admin_price_model"
	// ActionAdminPriceEdit picks which half of a model's price to change.
	ActionAdminPriceEdit Action = "admin_price_edit"
	// ActionAdminPriceApply writes the price the admin confirmed.
	ActionAdminPriceApply Action = "admin_price_apply"
	// ActionAdminPricesRefresh re-reads the upstream model catalogue.
	ActionAdminPricesRefresh Action = "admin_prices_refresh"
	// ActionAdminPricesPage pages through unpriced upstream models.
	ActionAdminPricesPage Action = "admin_prices_page"
	// ActionAdminPricesPricedPage pages through priced models.
	ActionAdminPricesPricedPage Action = "admin_prices_pp"
	// ActionAdminPriceAdd opens the "set both prices" flow for an unpriced
	// model the upstream serves.
	ActionAdminPriceAdd Action = "admin_price_add"
	// ActionAdminPriceAddApply adds that model to the price list.
	ActionAdminPriceAddApply Action = "admin_price_add_apply"
)

// Actor is the user asking. UserID is a Telegram user id.
type Actor struct {
	UserID  int64
	IsAdmin bool
}

// Resource is what the action touches. OwnerID is zero for shared resources.
type Resource struct {
	OwnerID int64
}

// Authorizer decides access. Implementations must be safe for concurrent use.
type Authorizer interface {
	Can(actor Actor, action Action, resource Resource) bool
}

// Rules is the default policy: admins may do anything, customers may act only
// on resources they own.
type Rules struct{}

// Can implements Authorizer.
func (Rules) Can(actor Actor, action Action, resource Resource) bool {
	if actor.UserID == 0 {
		return false
	}
	if actor.IsAdmin {
		return true
	}
	// An allow-list, deliberately: the old rule enumerated admin actions to
	// deny, so a new admin_* action that nobody remembered to add was instantly
	// usable by any customer on their own resource. Anything not named here is
	// admin-only by default.
	switch action {
	case ActionViewBalance, ActionViewUsage, ActionGenerateKey, ActionRevokeKey,
		ActionClaimTopUp, ActionConfirmClaim, ActionMenu, ActionListKeys,
		ActionViewPrices:
		// A customer action: still only ever on their own resource.
	default:
		return false
	}
	return resource.OwnerID != 0 && resource.OwnerID == actor.UserID
}

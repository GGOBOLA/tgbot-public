package authz

import "testing"

func TestRules(t *testing.T) {
	r := Rules{}
	admin := Actor{UserID: 1, IsAdmin: true}
	customer := Actor{UserID: 2}
	other := Resource{OwnerID: 3}
	own := Resource{OwnerID: 2}

	cases := []struct {
		name   string
		actor  Actor
		action Action
		res    Resource
		want   bool
	}{
		{"admin any action", admin, ActionAdminCredit, other, true},
		{"customer own key", customer, ActionGenerateKey, own, true},
		{"customer other's key", customer, ActionGenerateKey, other, false},
		{"customer admin credit", customer, ActionAdminCredit, own, false},
		{"anonymous denied", Actor{}, ActionViewBalance, own, false},
		{"zero owner denied", customer, ActionViewBalance, Resource{}, false},
	}
	for _, c := range cases {
		if got := r.Can(c.actor, c.action, c.res); got != c.want {
			t.Fatalf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// Every action is classified here once. A new admin action that nobody adds to
// the allow-list must be denied to customers by default, not allowed by default.
func TestCustomerAllowListIsExhaustive(t *testing.T) {
	customer := Actor{UserID: 7}
	own := Resource{OwnerID: 7}

	customerActions := map[Action]bool{
		ActionViewBalance: true, ActionViewUsage: true, ActionGenerateKey: true,
		ActionRevokeKey: true, ActionClaimTopUp: true, ActionConfirmClaim: true,
		ActionMenu: true, ActionListKeys: true, ActionViewPrices: true,
	}
	adminOnly := []Action{
		ActionAdminListUsers, ActionAdminCredit, ActionAdminPrices, ActionAdminPricesPriced, ActionAdminPricesUnpriced,
		ActionAdminPriceModel, ActionAdminPriceEdit, ActionAdminPriceApply,
		ActionAdminPricesRefresh, ActionAdminPricesPage, ActionAdminPricesPricedPage, ActionAdminPriceAdd,
		ActionAdminPriceAddApply, ActionAdminApproveClaim, ActionAdminRejectClaim,
		ActionAdminViewCustomer, ActionAdminToday,
	}

	for action, want := range customerActions {
		if got := (Rules{}).Can(customer, action, own); got != want {
			t.Fatalf("customer %s = %v, want %v", action, got, want)
		}
	}
	for _, action := range adminOnly {
		if (Rules{}).Can(customer, action, own) {
			t.Fatalf("customer allowed admin action %s", action)
		}
		if !(Rules{}).Can(Actor{UserID: 1, IsAdmin: true}, action, Resource{}) {
			t.Fatalf("admin denied admin action %s", action)
		}
	}
	// Unknown actions are admin-only, never customer-allowed.
	if (Rules{}).Can(customer, Action("admin_future_thing"), own) {
		t.Fatal("an unlisted action was allowed to a customer")
	}
}

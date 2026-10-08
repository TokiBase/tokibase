//go:build !no_payments

package payments

import "github.com/tokibase/tokibase/kernel"

func init() {
	kernel.RegisterModuleMarker("payments",
		[]string{"_payment_intents", "_payment_events", "_payment_products", "_refunds", "_subscriptions", "_entitlements"},
		[]string{"TOKI_PAYMENTS_MAYAR_API_KEY", "TOKI_PAYMENTS_PAYPAL_CLIENT_ID"}, false)
}

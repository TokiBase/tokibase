//go:build !no_payments

package tokibase

// Reference payment adapters register themselves with the payments module
// (they stay configured by env TOKI_PAYMENTS_<PROVIDER>_* only).
import (
	_ "github.com/tokibase/tokibase/modules/payments/providers/mayar"
	_ "github.com/tokibase/tokibase/modules/payments/providers/paypal"
)

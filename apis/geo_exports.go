package apis

import "github.com/tokibase/tokibase/core"

// CheckRecordsRateLimit applies the collection "list" rate limit rules
// (labels "<collection>:list", "*:list", ...) for modules that serve their own
// list-like endpoints (for example modules/geo).
func CheckRecordsRateLimit(e *core.RequestEvent, collection *core.Collection) error {
	return checkCollectionRateLimit(e, collection, "list")
}

// CheckSuperuserOnlyQueryFields rejects (403) non-superuser requests whose
// filter/sort expressions reference superuser-only fields (same check as the
// upstream records list endpoint).
func CheckSuperuserOnlyQueryFields(info *core.RequestInfo) error {
	return checkForSuperuserOnlyRuleFields(info)
}

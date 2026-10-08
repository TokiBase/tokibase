package apis

import "github.com/tokibase/tokibase/core"

// CheckRateLimitTags applies the rate limit rules that match any of the given
// labels (for example "payments:webhook") for modules that serve their own
// endpoints. Superusers and excluded IPs are skipped like everywhere else; the
// regular "METHOD /path" labels are still handled by the global middleware.
func CheckRateLimitTags(e *core.RequestEvent, tags ...string) error {
	if skipRateLimit(e) || len(tags) == 0 {
		return nil
	}
	rtId := e.Request.Pattern
	for _, t := range tags {
		rtId += t
	}
	rule, ok := e.App.Settings().RateLimits.FindRateLimitRule(tags, defaultRateLimitAudience(e)...)
	if ok {
		return checkRateLimit(e, rtId+rule.Audience, rule)
	}
	return nil
}

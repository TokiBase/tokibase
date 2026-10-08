package apis

import "github.com/tokibase/tokibase/core"

// SubmitOAuth2User runs the account mapping of the OAuth2 code flow (find or
// create the auth record, harden unverified records, link the _externalAuths
// row) for an already verified provider user. It lets modules such as
// nativeauth share one implementation with authWithOAuth2.
func SubmitOAuth2User(e *core.RecordAuthWithOAuth2RequestEvent, optExternalAuth *core.ExternalAuth) error {
	return oauth2Submit(e, optExternalAuth)
}

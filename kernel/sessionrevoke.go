package kernel

// RevokeSession is an optional seam that revokes the server-side session
// behind a token `sid` claim (and drops its realtime subscriptions). It is set
// by modules/sessions so that modules/kiosk can lock a device without
// importing it. A nil value means there is no session tracking. It reports
// whether an active session was revoked. Process wide, set once at startup.
var RevokeSession func(app App, sid, reason string) (bool, error)

// RevokeUserSessions is the companion seam that revokes every session of one
// auth record and returns how many were revoked. Same ownership as [RevokeSession].
var RevokeUserSessions func(app App, collection, userId, reason string) (int64, error)

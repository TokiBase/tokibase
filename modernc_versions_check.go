package tokibase

// ModerncDepsCheckHookId is the id of the hook that performs the modernc.org/* deps checks
// (the check itself lives in modules/store/sqlite).
// It could be used for removing/unbinding the hook if you don't want the checks.
const ModerncDepsCheckHookId = "pbModerncDepsCheck"

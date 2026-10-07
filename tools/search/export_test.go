package search

import "time"

// SetTimeNowForTest freezes the clock used by the time macros.
func SetTimeNowForTest(f func() time.Time) (restore func()) {
	prev := timeNow
	timeNow = f
	return func() { timeNow = prev }
}

// IdentifierMacroNames returns the registered macro identifiers.
func IdentifierMacroNames() []string {
	names := make([]string, 0, len(identifierMacros))
	for k := range identifierMacros {
		names = append(names, k)
	}
	return names
}

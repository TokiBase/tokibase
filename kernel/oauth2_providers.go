package kernel

// OAuth2ProviderRegistry resolves the available OAuth2 providers by name.
//
// The providers implementation lives in tools/auth (it talks HTTP) and
// therefore it can't be imported by the kernel. Instead, the kernel only
// needs to know which provider names are valid and this registry is the seam
// that the core package fills with the tools/auth providers.
type OAuth2ProviderRegistry interface {
	// Names returns the names of all registered providers.
	Names() []string

	// Exists checks whether a provider with the specified name is registered.
	Exists(name string) bool
}

// OAuth2Providers is the registry used by the kernel to validate OAuth2 provider names.
//
// By default no providers are registered (the core package replaces it on init).
var OAuth2Providers OAuth2ProviderRegistry = noOAuth2Providers{}

type noOAuth2Providers struct{}

func (noOAuth2Providers) Names() []string { return nil }

func (noOAuth2Providers) Exists(string) bool { return false }

package core

import (
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/auth"
)

func init() {
	kernel.OAuth2Providers = authProviders{}
}

// authProviders exposes the tools/auth providers to the kernel.
type authProviders struct{}

func (authProviders) Names() []string {
	names := make([]string, 0, len(auth.Providers))
	for name := range auth.Providers {
		names = append(names, name)
	}

	return names
}

func (authProviders) Exists(name string) bool {
	_, err := auth.NewProviderByName(name)

	return err == nil
}

// InitOAuth2Provider returns a new auth.Provider instance loaded with the provided OAuth2ProviderConfig options.
//
// (it replaces the former OAuth2ProviderConfig.InitProvider method,
// which can't live in the kernel because auth.Provider depends on net/http)
func InitOAuth2Provider(c OAuth2ProviderConfig) (auth.Provider, error) {
	provider, err := auth.NewProviderByName(c.Name)
	if err != nil {
		return nil, err
	}

	if c.ClientId != "" {
		provider.SetClientId(c.ClientId)
	}

	if c.ClientSecret != "" {
		provider.SetClientSecret(c.ClientSecret)
	}

	if c.AuthURL != "" {
		provider.SetAuthURL(c.AuthURL)
	}

	if c.UserInfoURL != "" {
		provider.SetUserInfoURL(c.UserInfoURL)
	}

	if c.TokenURL != "" {
		provider.SetTokenURL(c.TokenURL)
	}

	if c.DisplayName != "" {
		provider.SetDisplayName(c.DisplayName)
	}

	if c.PKCE != nil {
		provider.SetPKCE(*c.PKCE)
	}

	if c.Extra != nil {
		provider.SetExtra(c.Extra)
	}

	return provider, nil
}

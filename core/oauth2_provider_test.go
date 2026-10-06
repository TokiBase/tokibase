package core_test

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"testing"

	"github.com/tokibase/tokibase/tools/types"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/auth"
)

func TestOAuth2ProviderConfigInitProvider(t *testing.T) {
	scenarios := []struct {
		name           string
		config         kernel.OAuth2ProviderConfig
		expectedConfig kernel.OAuth2ProviderConfig
		expectedError  bool
	}{
		{
			"empty config",
			kernel.OAuth2ProviderConfig{},
			kernel.OAuth2ProviderConfig{},
			true,
		},
		{
			"missing provider",
			kernel.OAuth2ProviderConfig{
				Name:         "missing",
				ClientId:     "test_ClientId",
				ClientSecret: "test_ClientSecret",
				AuthURL:      "test_AuthURL",
				TokenURL:     "test_TokenURL",
				UserInfoURL:  "test_UserInfoURL",
				DisplayName:  "test_DisplayName",
				PKCE:         types.Pointer(true),
			},
			kernel.OAuth2ProviderConfig{
				Name:         "missing",
				ClientId:     "test_ClientId",
				ClientSecret: "test_ClientSecret",
				AuthURL:      "test_AuthURL",
				TokenURL:     "test_TokenURL",
				UserInfoURL:  "test_UserInfoURL",
				DisplayName:  "test_DisplayName",
				PKCE:         types.Pointer(true),
			},
			true,
		},
		{
			"existing provider minimal",
			kernel.OAuth2ProviderConfig{
				Name: "gitlab",
			},
			kernel.OAuth2ProviderConfig{
				Name:         "gitlab",
				ClientId:     "",
				ClientSecret: "",
				AuthURL:      "https://gitlab.com/oauth/authorize",
				TokenURL:     "https://gitlab.com/oauth/token",
				UserInfoURL:  "https://gitlab.com/api/v4/user",
				DisplayName:  "GitLab",
				PKCE:         types.Pointer(true),
			},
			false,
		},
		{
			"existing provider with all fields",
			kernel.OAuth2ProviderConfig{
				Name:         "gitlab",
				ClientId:     "test_ClientId",
				ClientSecret: "test_ClientSecret",
				AuthURL:      "test_AuthURL",
				TokenURL:     "test_TokenURL",
				UserInfoURL:  "test_UserInfoURL",
				DisplayName:  "test_DisplayName",
				PKCE:         types.Pointer(true),
				Extra:        map[string]any{"a": 1},
			},
			kernel.OAuth2ProviderConfig{
				Name:         "gitlab",
				ClientId:     "test_ClientId",
				ClientSecret: "test_ClientSecret",
				AuthURL:      "test_AuthURL",
				TokenURL:     "test_TokenURL",
				UserInfoURL:  "test_UserInfoURL",
				DisplayName:  "test_DisplayName",
				PKCE:         types.Pointer(true),
				Extra:        map[string]any{"a": 1},
			},
			false,
		},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			provider, err := core.InitOAuth2Provider(s.config)

			hasErr := err != nil
			if hasErr != s.expectedError {
				t.Fatalf("Expected hasErr %v, got %v", s.expectedError, hasErr)
			}

			if hasErr {
				if provider != nil {
					t.Fatalf("Expected nil provider, got %v", provider)
				}
				return
			}

			factory, ok := auth.Providers[s.expectedConfig.Name]
			if !ok {
				t.Fatalf("Missing factory for provider %q", s.expectedConfig.Name)
			}

			expectedType := fmt.Sprintf("%T", factory())
			providerType := fmt.Sprintf("%T", provider)
			if expectedType != providerType {
				t.Fatalf("Expected provider instanceof %q, got %q", expectedType, providerType)
			}

			if provider.ClientId() != s.expectedConfig.ClientId {
				t.Fatalf("Expected ClientId %q, got %q", s.expectedConfig.ClientId, provider.ClientId())
			}

			if provider.ClientSecret() != s.expectedConfig.ClientSecret {
				t.Fatalf("Expected ClientSecret %q, got %q", s.expectedConfig.ClientSecret, provider.ClientSecret())
			}

			if provider.AuthURL() != s.expectedConfig.AuthURL {
				t.Fatalf("Expected AuthURL %q, got %q", s.expectedConfig.AuthURL, provider.AuthURL())
			}

			if provider.UserInfoURL() != s.expectedConfig.UserInfoURL {
				t.Fatalf("Expected UserInfoURL %q, got %q", s.expectedConfig.UserInfoURL, provider.UserInfoURL())
			}

			if provider.TokenURL() != s.expectedConfig.TokenURL {
				t.Fatalf("Expected TokenURL %q, got %q", s.expectedConfig.TokenURL, provider.TokenURL())
			}

			if provider.DisplayName() != s.expectedConfig.DisplayName {
				t.Fatalf("Expected DisplayName %q, got %q", s.expectedConfig.DisplayName, provider.DisplayName())
			}

			if provider.PKCE() != *s.expectedConfig.PKCE {
				t.Fatalf("Expected PKCE %v, got %v", *s.expectedConfig.PKCE, provider.PKCE())
			}

			rawMeta, _ := json.Marshal(provider.Extra(), json.Deterministic(true))
			expectedMeta, _ := json.Marshal(s.expectedConfig.Extra, json.Deterministic(true))
			if !bytes.Equal(rawMeta, expectedMeta) {
				t.Fatalf("Expected PKCE %v, got %v", *s.expectedConfig.PKCE, provider.PKCE())
			}
		})
	}
}

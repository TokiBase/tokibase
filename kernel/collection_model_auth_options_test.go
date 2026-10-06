package kernel_test

import (
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tests"
	"github.com/tokibase/tokibase/tools/types"
)

func TestCollectionAuthOptionsValidate(t *testing.T) {
	t.Parallel()

	scenarios := []struct {
		name           string
		collection     func(app kernel.App) (*kernel.Collection, error)
		expectedErrors []string
	}{
		// authRule
		{
			name: "nil authRule",
			collection: func(app kernel.App) (*kernel.Collection, error) {
				c := kernel.NewAuthCollection("new_auth")
				c.AuthRule = nil
				return c, nil
			},
			expectedErrors: []string{},
		},
		{
			name: "empty authRule",
			collection: func(app kernel.App) (*kernel.Collection, error) {
				c := kernel.NewAuthCollection("new_auth")
				c.AuthRule = types.Pointer("")
				return c, nil
			},
			expectedErrors: []string{},
		},
		{
			name: "invalid authRule",
			collection: func(app kernel.App) (*kernel.Collection, error) {
				c := kernel.NewAuthCollection("new_auth")
				c.AuthRule = types.Pointer("missing != ''")
				return c, nil
			},
			expectedErrors: []string{"authRule"},
		},
		{
			name: "valid authRule",
			collection: func(app kernel.App) (*kernel.Collection, error) {
				c := kernel.NewAuthCollection("new_auth")
				c.AuthRule = types.Pointer("id != ''")
				return c, nil
			},
			expectedErrors: []string{},
		},

		// manageRule
		{
			name: "nil manageRule",
			collection: func(app kernel.App) (*kernel.Collection, error) {
				c := kernel.NewAuthCollection("new_auth")
				c.ManageRule = nil
				return c, nil
			},
			expectedErrors: []string{},
		},
		{
			name: "empty manageRule",
			collection: func(app kernel.App) (*kernel.Collection, error) {
				c := kernel.NewAuthCollection("new_auth")
				c.ManageRule = types.Pointer("")
				return c, nil
			},
			expectedErrors: []string{"manageRule"},
		},
		{
			name: "invalid manageRule",
			collection: func(app kernel.App) (*kernel.Collection, error) {
				c := kernel.NewAuthCollection("new_auth")
				c.ManageRule = types.Pointer("missing != ''")
				return c, nil
			},
			expectedErrors: []string{"manageRule"},
		},
		{
			name: "valid manageRule",
			collection: func(app kernel.App) (*kernel.Collection, error) {
				c := kernel.NewAuthCollection("new_auth")
				c.ManageRule = types.Pointer("id != ''")
				return c, nil
			},
			expectedErrors: []string{},
		},

		// passwordAuth
		{
			name: "trigger passwordAuth validations",
			collection: func(app kernel.App) (*kernel.Collection, error) {
				c := kernel.NewAuthCollection("new_auth")
				c.PasswordAuth = kernel.PasswordAuthConfig{
					Enabled: true,
				}
				return c, nil
			},
			expectedErrors: []string{"passwordAuth"},
		},
		{
			name: "passwordAuth with non-unique identity fields",
			collection: func(app kernel.App) (*kernel.Collection, error) {
				c := kernel.NewAuthCollection("new_auth")
				c.Fields.Add(&kernel.TextField{Name: "test"})
				c.PasswordAuth = kernel.PasswordAuthConfig{
					Enabled:        true,
					IdentityFields: []string{"email", "test"},
				}
				return c, nil
			},
			expectedErrors: []string{"passwordAuth"},
		},
		{
			name: "passwordAuth with non-unique identity fields",
			collection: func(app kernel.App) (*kernel.Collection, error) {
				c := kernel.NewAuthCollection("new_auth")
				c.Fields.Add(&kernel.TextField{Name: "test"})
				c.AddIndex("auth_test_idx", true, "test", "")
				c.PasswordAuth = kernel.PasswordAuthConfig{
					Enabled:        true,
					IdentityFields: []string{"email", "test"},
				}
				return c, nil
			},
			expectedErrors: []string{},
		},

		// oauth2
		{
			name: "trigger oauth2 validations",
			collection: func(app kernel.App) (*kernel.Collection, error) {
				c := kernel.NewAuthCollection("new_auth")
				c.OAuth2 = kernel.OAuth2Config{
					Enabled: true,
					Providers: []kernel.OAuth2ProviderConfig{
						{Name: "missing"},
					},
				}
				return c, nil
			},
			expectedErrors: []string{"oauth2"},
		},

		// otp
		{
			name: "trigger otp validations",
			collection: func(app kernel.App) (*kernel.Collection, error) {
				c := kernel.NewAuthCollection("new_auth")
				c.OTP = kernel.OTPConfig{
					Enabled:  true,
					Duration: -10,
				}
				return c, nil
			},
			expectedErrors: []string{"otp"},
		},

		// mfa
		{
			name: "trigger mfa validations",
			collection: func(app kernel.App) (*kernel.Collection, error) {
				c := kernel.NewAuthCollection("new_auth")
				c.MFA = kernel.MFAConfig{
					Enabled:  true,
					Duration: -10,
				}
				return c, nil
			},
			expectedErrors: []string{"mfa"},
		},
		{
			name: "mfa enabled with < 2 auth methods",
			collection: func(app kernel.App) (*kernel.Collection, error) {
				c := kernel.NewAuthCollection("new_auth")
				c.MFA.Enabled = true
				c.PasswordAuth.Enabled = true
				c.OTP.Enabled = false
				c.OAuth2.Enabled = false
				return c, nil
			},
			expectedErrors: []string{"mfa"},
		},
		{
			name: "mfa enabled with >= 2 auth methods",
			collection: func(app kernel.App) (*kernel.Collection, error) {
				c := kernel.NewAuthCollection("new_auth")
				c.MFA.Enabled = true
				c.PasswordAuth.Enabled = true
				c.OTP.Enabled = true
				c.OAuth2.Enabled = false
				return c, nil
			},
			expectedErrors: []string{},
		},
		{
			name: "mfa disabled with invalid rule",
			collection: func(app kernel.App) (*kernel.Collection, error) {
				c := kernel.NewAuthCollection("new_auth")
				c.PasswordAuth.Enabled = true
				c.OTP.Enabled = true
				c.MFA.Enabled = false
				c.MFA.Rule = "invalid"
				return c, nil
			},
			expectedErrors: []string{},
		},
		{
			name: "mfa enabled with invalid rule",
			collection: func(app kernel.App) (*kernel.Collection, error) {
				c := kernel.NewAuthCollection("new_auth")
				c.PasswordAuth.Enabled = true
				c.OTP.Enabled = true
				c.MFA.Enabled = true
				c.MFA.Rule = "invalid"
				return c, nil
			},
			expectedErrors: []string{"mfa"},
		},
		{
			name: "mfa enabled with valid rule",
			collection: func(app kernel.App) (*kernel.Collection, error) {
				c := kernel.NewAuthCollection("new_auth")
				c.PasswordAuth.Enabled = true
				c.OTP.Enabled = true
				c.MFA.Enabled = true
				c.MFA.Rule = "1=1"
				return c, nil
			},
			expectedErrors: []string{},
		},

		// tokens
		{
			name: "trigger authToken validations",
			collection: func(app kernel.App) (*kernel.Collection, error) {
				c := kernel.NewAuthCollection("new_auth")
				c.AuthToken.Secret = ""
				return c, nil
			},
			expectedErrors: []string{"authToken"},
		},
		{
			name: "trigger passwordResetToken validations",
			collection: func(app kernel.App) (*kernel.Collection, error) {
				c := kernel.NewAuthCollection("new_auth")
				c.PasswordResetToken.Secret = ""
				return c, nil
			},
			expectedErrors: []string{"passwordResetToken"},
		},
		{
			name: "trigger emailChangeToken validations",
			collection: func(app kernel.App) (*kernel.Collection, error) {
				c := kernel.NewAuthCollection("new_auth")
				c.EmailChangeToken.Secret = ""
				return c, nil
			},
			expectedErrors: []string{"emailChangeToken"},
		},
		{
			name: "trigger verificationToken validations",
			collection: func(app kernel.App) (*kernel.Collection, error) {
				c := kernel.NewAuthCollection("new_auth")
				c.VerificationToken.Secret = ""
				return c, nil
			},
			expectedErrors: []string{"verificationToken"},
		},
		{
			name: "trigger fileToken validations",
			collection: func(app kernel.App) (*kernel.Collection, error) {
				c := kernel.NewAuthCollection("new_auth")
				c.FileToken.Secret = ""
				return c, nil
			},
			expectedErrors: []string{"fileToken"},
		},

		// templates
		{
			name: "trigger verificationTemplate validations",
			collection: func(app kernel.App) (*kernel.Collection, error) {
				c := kernel.NewAuthCollection("new_auth")
				c.VerificationTemplate.Body = ""
				return c, nil
			},
			expectedErrors: []string{"verificationTemplate"},
		},
		{
			name: "trigger resetPasswordTemplate validations",
			collection: func(app kernel.App) (*kernel.Collection, error) {
				c := kernel.NewAuthCollection("new_auth")
				c.ResetPasswordTemplate.Body = ""
				return c, nil
			},
			expectedErrors: []string{"resetPasswordTemplate"},
		},
		{
			name: "trigger confirmEmailChangeTemplate validations",
			collection: func(app kernel.App) (*kernel.Collection, error) {
				c := kernel.NewAuthCollection("new_auth")
				c.ConfirmEmailChangeTemplate.Body = ""
				return c, nil
			},
			expectedErrors: []string{"confirmEmailChangeTemplate"},
		},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			app, _ := tests.NewTestApp()
			defer app.Cleanup()

			collection, err := s.collection(app)
			if err != nil {
				t.Fatalf("Failed to retrieve test collection: %v", err)
			}

			result := app.Validate(collection)

			tests.TestValidationErrors(t, result, s.expectedErrors)
		})
	}
}

func TestEmailTemplateValidate(t *testing.T) {
	scenarios := []struct {
		name           string
		template       kernel.EmailTemplate
		expectedErrors []string
	}{
		{
			"zero value",
			kernel.EmailTemplate{},
			[]string{"subject", "body"},
		},
		{
			"non-empty data",
			kernel.EmailTemplate{
				Subject: "a",
				Body:    "b",
			},
			[]string{},
		},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			result := s.template.Validate()

			tests.TestValidationErrors(t, result, s.expectedErrors)
		})
	}
}

func TestEmailTemplateResolve(t *testing.T) {
	template := kernel.EmailTemplate{
		Subject: "test_subject {PARAM3} {PARAM1}-{PARAM2} repeat-{PARAM1}",
		Body:    "test_body {PARAM3} {PARAM2}-{PARAM1} repeat-{PARAM2}",
	}

	scenarios := []struct {
		name            string
		placeholders    map[string]any
		template        kernel.EmailTemplate
		expectedSubject string
		expectedBody    string
	}{
		{
			"no placeholders",
			nil,
			template,
			template.Subject,
			template.Body,
		},
		{
			"no matching placeholders",
			map[string]any{"{A}": "abc", "{B}": 456},
			template,
			template.Subject,
			template.Body,
		},
		{
			"at least one matching placeholder",
			map[string]any{"{PARAM1}": "abc", "{PARAM2}": 456},
			template,
			"test_subject {PARAM3} abc-456 repeat-abc",
			"test_body {PARAM3} 456-abc repeat-456",
		},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			subject, body := s.template.Resolve(s.placeholders)

			if subject != s.expectedSubject {
				t.Fatalf("Expected subject\n%v\ngot\n%v", s.expectedSubject, subject)
			}

			if body != s.expectedBody {
				t.Fatalf("Expected body\n%v\ngot\n%v", s.expectedBody, body)
			}
		})
	}
}

func TestTokenConfigValidate(t *testing.T) {
	scenarios := []struct {
		name           string
		config         kernel.TokenConfig
		expectedErrors []string
	}{
		{
			"zero value",
			kernel.TokenConfig{},
			[]string{"secret", "duration"},
		},
		{
			"invalid data",
			kernel.TokenConfig{
				Secret:   strings.Repeat("a", 29),
				Duration: 9,
			},
			[]string{"secret", "duration"},
		},
		{
			"valid data",
			kernel.TokenConfig{
				Secret:   strings.Repeat("a", 30),
				Duration: 10,
			},
			[]string{},
		},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			result := s.config.Validate()

			tests.TestValidationErrors(t, result, s.expectedErrors)
		})
	}
}

func TestTokenConfigDurationTime(t *testing.T) {
	scenarios := []struct {
		config   kernel.TokenConfig
		expected time.Duration
	}{
		{kernel.TokenConfig{}, 0 * time.Second},
		{kernel.TokenConfig{Duration: 1234}, 1234 * time.Second},
	}

	for i, s := range scenarios {
		t.Run(fmt.Sprintf("%d_%d", i, s.config.Duration), func(t *testing.T) {
			result := s.config.DurationTime()

			if result != s.expected {
				t.Fatalf("Expected duration %d, got %d", s.expected, result)
			}
		})
	}
}

func TestAuthAlertConfigValidate(t *testing.T) {
	scenarios := []struct {
		name           string
		config         kernel.AuthAlertConfig
		expectedErrors []string
	}{
		{
			"zero value (disabled)",
			kernel.AuthAlertConfig{},
			[]string{"emailTemplate"},
		},
		{
			"zero value (enabled)",
			kernel.AuthAlertConfig{Enabled: true},
			[]string{"emailTemplate"},
		},
		{
			"invalid template",
			kernel.AuthAlertConfig{
				EmailTemplate: kernel.EmailTemplate{Body: "", Subject: "b"},
			},
			[]string{"emailTemplate"},
		},
		{
			"valid data",
			kernel.AuthAlertConfig{
				EmailTemplate: kernel.EmailTemplate{Body: "a", Subject: "b"},
			},
			[]string{},
		},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			result := s.config.Validate()

			tests.TestValidationErrors(t, result, s.expectedErrors)
		})
	}
}

func TestOTPConfigValidate(t *testing.T) {
	scenarios := []struct {
		name           string
		config         kernel.OTPConfig
		expectedErrors []string
	}{
		{
			"zero value (disabled)",
			kernel.OTPConfig{},
			[]string{"emailTemplate"},
		},
		{
			"zero value (enabled)",
			kernel.OTPConfig{Enabled: true},
			[]string{"duration", "length", "emailTemplate"},
		},
		{
			"invalid length (< 3)",
			kernel.OTPConfig{
				Enabled:       true,
				EmailTemplate: kernel.EmailTemplate{Body: "a", Subject: "b"},
				Duration:      100,
				Length:        3,
			},
			[]string{"length"},
		},
		{
			"invalid duration (< 10)",
			kernel.OTPConfig{
				Enabled:       true,
				EmailTemplate: kernel.EmailTemplate{Body: "a", Subject: "b"},
				Duration:      9,
				Length:        100,
			},
			[]string{"duration"},
		},
		{
			"invalid duration (> 86400)",
			kernel.OTPConfig{
				Enabled:       true,
				EmailTemplate: kernel.EmailTemplate{Body: "a", Subject: "b"},
				Duration:      86401,
				Length:        100,
			},
			[]string{"duration"},
		},
		{
			"invalid template (triggering EmailTemplate validations)",
			kernel.OTPConfig{
				Enabled:       true,
				EmailTemplate: kernel.EmailTemplate{Body: "", Subject: "b"},
				Duration:      86400,
				Length:        4,
			},
			[]string{"emailTemplate"},
		},
		{
			"valid data",
			kernel.OTPConfig{
				Enabled:       true,
				EmailTemplate: kernel.EmailTemplate{Body: "a", Subject: "b"},
				Duration:      86400,
				Length:        4,
			},
			[]string{},
		},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			result := s.config.Validate()

			tests.TestValidationErrors(t, result, s.expectedErrors)
		})
	}
}

func TestOTPConfigDurationTime(t *testing.T) {
	scenarios := []struct {
		config   kernel.OTPConfig
		expected time.Duration
	}{
		{kernel.OTPConfig{}, 0 * time.Second},
		{kernel.OTPConfig{Duration: 1234}, 1234 * time.Second},
	}

	for i, s := range scenarios {
		t.Run(fmt.Sprintf("%d_%d", i, s.config.Duration), func(t *testing.T) {
			result := s.config.DurationTime()

			if result != s.expected {
				t.Fatalf("Expected duration %d, got %d", s.expected, result)
			}
		})
	}
}

func TestMFAConfigValidate(t *testing.T) {
	scenarios := []struct {
		name           string
		config         kernel.MFAConfig
		expectedErrors []string
	}{
		{
			"zero value (disabled)",
			kernel.MFAConfig{},
			[]string{},
		},
		{
			"zero value (enabled)",
			kernel.MFAConfig{Enabled: true},
			[]string{"duration"},
		},
		{
			"invalid duration (< 10)",
			kernel.MFAConfig{Enabled: true, Duration: 9},
			[]string{"duration"},
		},
		{
			"invalid duration (> 86400)",
			kernel.MFAConfig{Enabled: true, Duration: 86401},
			[]string{"duration"},
		},
		{
			"valid data",
			kernel.MFAConfig{Enabled: true, Duration: 86400},
			[]string{},
		},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			result := s.config.Validate()

			tests.TestValidationErrors(t, result, s.expectedErrors)
		})
	}
}

func TestMFAConfigDurationTime(t *testing.T) {
	scenarios := []struct {
		config   kernel.MFAConfig
		expected time.Duration
	}{
		{kernel.MFAConfig{}, 0 * time.Second},
		{kernel.MFAConfig{Duration: 1234}, 1234 * time.Second},
	}

	for i, s := range scenarios {
		t.Run(fmt.Sprintf("%d_%d", i, s.config.Duration), func(t *testing.T) {
			result := s.config.DurationTime()

			if result != s.expected {
				t.Fatalf("Expected duration %d, got %d", s.expected, result)
			}
		})
	}
}

func TestPasswordAuthConfigValidate(t *testing.T) {
	scenarios := []struct {
		name           string
		config         kernel.PasswordAuthConfig
		expectedErrors []string
	}{
		{
			"zero value (disabled)",
			kernel.PasswordAuthConfig{},
			[]string{},
		},
		{
			"zero value (enabled)",
			kernel.PasswordAuthConfig{Enabled: true},
			[]string{"identityFields"},
		},
		{
			"empty values",
			kernel.PasswordAuthConfig{Enabled: true, IdentityFields: []string{"", ""}},
			[]string{"identityFields"},
		},
		{
			"valid data",
			kernel.PasswordAuthConfig{Enabled: true, IdentityFields: []string{"abc"}},
			[]string{},
		},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			result := s.config.Validate()

			tests.TestValidationErrors(t, result, s.expectedErrors)
		})
	}
}

func TestOAuth2ConfigUnmarshalJSON(t *testing.T) {
	t.Parallel()

	scenarios := []struct {
		name     string
		newJSON  string
		expected string
	}{
		{
			"missing",
			`{
				"enabled": true,
				"mappedFields": {"username": "username_test"}
			}`,
			`{"providers":[{"pkce":null,"name":"a","clientId":"a_clientId","clientSecret":"a_clientSecret","authURL":"","tokenURL":"","userInfoURL":"","displayName":"","extra":{}},{"pkce":null,"name":"b","clientId":"b_clientId","clientSecret":"b_clientSecret","authURL":"","tokenURL":"","userInfoURL":"","displayName":"","extra":{}}],"mappedFields":{"id":"","name":"name_test","username":"username_test","avatarURL":""},"enabled":true}`,
		},
		{
			"empty",
			`{
				"enabled": true,
				"mappedFields": {"username": "username_test"},
				"providers": []
			}`,
			`{"providers":[],"mappedFields":{"id":"","name":"name_test","username":"username_test","avatarURL":""},"enabled":true}`,
		},
		{
			"non-empty",
			`{
				"enabled": true,
				"mappedFields": {"username": "username_test"},
				"providers": [
					{"name": "c", "clientId": "c_clientId", "clientSecret": "c_clientSecret"},
					{"name": "a", "displayName": "a_displayName"}
				]
			}`,
			`{"providers":[{"pkce":null,"name":"c","clientId":"c_clientId","clientSecret":"c_clientSecret","authURL":"","tokenURL":"","userInfoURL":"","displayName":"","extra":{}},{"pkce":null,"name":"a","clientId":"a_clientId","clientSecret":"a_clientSecret","authURL":"","tokenURL":"","userInfoURL":"","displayName":"a_displayName","extra":{}}],"mappedFields":{"id":"","name":"name_test","username":"username_test","avatarURL":""},"enabled":true}`,
		},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			config := kernel.OAuth2Config{
				Enabled: false,
				MappedFields: kernel.OAuth2KnownFields{
					Name: "name_test",
				},
				Providers: []kernel.OAuth2ProviderConfig{
					{Name: "a", ClientId: "a_clientId", ClientSecret: "a_clientSecret"},
					{Name: "b", ClientId: "b_clientId", ClientSecret: "b_clientSecret"},
				},
			}

			err := json.Unmarshal([]byte(s.newJSON), &config)
			if err != nil {
				t.Fatal(err)
			}

			raw, err := json.Marshal(config, json.Deterministic(true))
			if err != nil {
				t.Fatal(err)
			}
			rawStr := string(raw)

			if rawStr != s.expected {
				t.Fatalf("Expected OAuth2ProviderConfig\n%s\ngot\n%s", s.expected, rawStr)
			}
		})
	}
}

func TestOAuth2ConfigGetProviderConfig(t *testing.T) {
	scenarios := []struct {
		name           string
		providerName   string
		config         kernel.OAuth2Config
		expectedExists bool
	}{
		{
			"zero value",
			"gitlab",
			kernel.OAuth2Config{},
			false,
		},
		{
			"empty config with valid provider",
			"gitlab",
			kernel.OAuth2Config{},
			false,
		},
		{
			"non-empty config with missing provider",
			"gitlab",
			kernel.OAuth2Config{Providers: []kernel.OAuth2ProviderConfig{{Name: "google"}, {Name: "github"}}},
			false,
		},
		{
			"config with existing provider",
			"github",
			kernel.OAuth2Config{Providers: []kernel.OAuth2ProviderConfig{{Name: "google"}, {Name: "github"}}},
			true,
		},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			config, exists := s.config.GetProviderConfig(s.providerName)

			if exists != s.expectedExists {
				t.Fatalf("Expected exists %v, got %v", s.expectedExists, exists)
			}

			if exists {
				if config.Name != s.providerName {
					t.Fatalf("Expected config with name %q, got %q", s.providerName, config.Name)
				}
			} else {
				if config.Name != "" {
					t.Fatalf("Expected empty config, got %v", config)
				}
			}
		})
	}
}

func TestOAuth2ConfigValidate(t *testing.T) {
	scenarios := []struct {
		name           string
		config         kernel.OAuth2Config
		expectedErrors []string
	}{
		{
			"zero value (disabled)",
			kernel.OAuth2Config{},
			[]string{},
		},
		{
			"zero value (enabled)",
			kernel.OAuth2Config{Enabled: true},
			[]string{},
		},
		{
			"unknown provider",
			kernel.OAuth2Config{Enabled: true, Providers: []kernel.OAuth2ProviderConfig{
				{Name: "missing", ClientId: "abc", ClientSecret: "456"},
			}},
			[]string{"providers"},
		},
		{
			"known provider with invalid data",
			kernel.OAuth2Config{Enabled: true, Providers: []kernel.OAuth2ProviderConfig{
				{Name: "gitlab", ClientId: "abc", TokenURL: "!invalid!"},
			}},
			[]string{"providers"},
		},
		{
			"known provider with valid data",
			kernel.OAuth2Config{Enabled: true, Providers: []kernel.OAuth2ProviderConfig{
				{Name: "gitlab", ClientId: "abc", ClientSecret: "456", TokenURL: "https://example.com"},
			}},
			[]string{},
		},
		{
			"known provider with valid data (duplicated)",
			kernel.OAuth2Config{Enabled: true, Providers: []kernel.OAuth2ProviderConfig{
				{Name: "gitlab", ClientId: "abc1", ClientSecret: "1", TokenURL: "https://example1.com"},
				{Name: "google", ClientId: "abc2", ClientSecret: "2", TokenURL: "https://example2.com"},
				{Name: "gitlab", ClientId: "abc3", ClientSecret: "3", TokenURL: "https://example3.com"},
			}},
			[]string{"providers"},
		},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			result := s.config.Validate()

			tests.TestValidationErrors(t, result, s.expectedErrors)
		})
	}
}

func TestOAuth2ProviderConfigValidate(t *testing.T) {
	scenarios := []struct {
		name           string
		config         kernel.OAuth2ProviderConfig
		expectedErrors []string
	}{
		{
			"zero value",
			kernel.OAuth2ProviderConfig{},
			[]string{"name", "clientId", "clientSecret"},
		},
		{
			"minimum valid data",
			kernel.OAuth2ProviderConfig{Name: "gitlab", ClientId: "abc", ClientSecret: "456"},
			[]string{},
		},
		{
			"non-existing provider",
			kernel.OAuth2ProviderConfig{Name: "missing", ClientId: "abc", ClientSecret: "456"},
			[]string{"name"},
		},
		{
			"invalid urls",
			kernel.OAuth2ProviderConfig{
				Name:         "gitlab",
				ClientId:     "abc",
				ClientSecret: "456",
				AuthURL:      "!invalid!",
				TokenURL:     "!invalid!",
				UserInfoURL:  "!invalid!",
			},
			[]string{"authURL", "tokenURL", "userInfoURL"},
		},
		{
			"valid urls",
			kernel.OAuth2ProviderConfig{
				Name:         "gitlab",
				ClientId:     "abc",
				ClientSecret: "456",
				AuthURL:      "https://example.com/a",
				TokenURL:     "https://example.com/b",
				UserInfoURL:  "https://example.com/c",
			},
			[]string{},
		},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			result := s.config.Validate()

			tests.TestValidationErrors(t, result, s.expectedErrors)
		})
	}
}

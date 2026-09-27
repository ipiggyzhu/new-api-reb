package oauth

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

// Provider defines the interface for OAuth providers
type Provider interface {
	// GetName returns the display name of the provider (e.g., "GitHub", "Discord")
	GetName() string

	// IsEnabled returns whether this OAuth provider is enabled
	IsEnabled() bool

	// ExchangeToken exchanges the authorization code for an access token
	// The gin.Context is passed for providers that need request info (e.g., for redirect_uri)
	ExchangeToken(ctx context.Context, code string, c *gin.Context) (*OAuthToken, error)

	// GetUserInfo retrieves user information using the access token
	GetUserInfo(ctx context.Context, token *OAuthToken) (*OAuthUser, error)

	// IsUserIDTaken checks if the provider user ID is already associated with an account
	IsUserIDTaken(providerUserID string) bool

	// FillUserByProviderID fills the user model by provider user ID
	FillUserByProviderID(user *model.User, providerUserID string) error

	// SetProviderUserID sets the provider user ID on the user model
	SetProviderUserID(user *model.User, providerUserID string)

	// GetProviderPrefix returns the prefix for auto-generated usernames (e.g., "github_")
	GetProviderPrefix() string
}

// oauthDo sends an OAuth provider HTTP request with the given per-provider
// timeout and normalizes a transport failure into the shared connect-failed
// OAuth error, logging it under logTag. On success it returns the live response;
// the caller owns closing res.Body.
func oauthDo(ctx context.Context, req *http.Request, timeout time.Duration, logTag, providerName string) (*http.Response, error) {
	client := http.Client{Timeout: timeout}
	res, err := client.Do(req)
	if err != nil {
		logger.LogError(ctx, fmt.Sprintf("%s error: %s", logTag, err.Error()))
		return nil, NewOAuthErrorWithRaw(i18n.MsgOAuthConnectFailed, map[string]any{"Provider": providerName}, err.Error())
	}
	return res, nil
}

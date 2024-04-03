package sink

import (
	"github.com/streamingfast/substreams/client"
	"os"
)

type subsAuthenticator struct {
	apiKeyEnvVar   string
	apiTokenEnvVar string
}

func NewSubsAuthenticator(apiKeyEnvVar string, apiTokenEnvVar string) *subsAuthenticator {
	return &subsAuthenticator{
		apiKeyEnvVar:   apiKeyEnvVar,
		apiTokenEnvVar: apiTokenEnvVar,
	}
}

func (a *subsAuthenticator) GetApiKey() string {
	return a.apiKeyEnvVar
}

func (a *subsAuthenticator) GetApiToken() string {
	return a.apiTokenEnvVar
}

func (a *subsAuthenticator) GetAuth() (authToken string, authType client.AuthType) {
	apiKeyFromEnv := os.Getenv(a.apiKeyEnvVar)
	if apiKeyFromEnv != "" {
		return apiKeyFromEnv, client.ApiKey
	}

	apiTokenFromEnv := os.Getenv(a.apiTokenEnvVar)
	if apiTokenFromEnv != "" {
		return apiTokenFromEnv, client.JWT
	}
	return "", client.None
}

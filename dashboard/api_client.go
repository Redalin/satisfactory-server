package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// APIClient queries the Satisfactory 1.0 Dedicated Server HTTPS API
type APIClient struct {
	baseURL        string
	apiToken       string
	serverPassword string
	sessionToken   string
	mu             sync.Mutex
	httpClient     *http.Client
}

// ServerHealthResponse represents the JSON payload from HealthCheck
type ServerHealthResponse struct {
	Data struct {
		Health string `json:"health"`
	} `json:"data"`
}

// ServerStateResponse represents the JSON payload from QueryServerState
type ServerStateResponse struct {
	Data struct {
		ServerName      string `json:"serverName"`
		ServerGameState struct {
			ServerName           string  `json:"serverName"`
			ActiveSessionName    string  `json:"activeSessionName"`
			NumConnectedPlayers  int     `json:"numConnectedPlayers"`
			PlayerLimit          int     `json:"playerLimit"`
			TechTier             int     `json:"techTier"`
			ActiveSchematic      string  `json:"activeSchematic"`
			GamePhase            string  `json:"gamePhase"`
			IsGameRunning        bool    `json:"isGameRunning"`
			TotalGameDuration    float64 `json:"totalGameDuration"`
			IsAutoSaveInProgress bool    `json:"isAutoSaveInProgress"`
			AverageTickRate      float64 `json:"averageTickRate"`
		} `json:"serverGameState"`
	} `json:"data"`
}

// ServerOptionsResponse represents the JSON payload from GetServerOptions
type ServerOptionsResponse struct {
	Data struct {
		ServerOptions        map[string]string `json:"serverOptions"`
		PendingServerOptions map[string]string `json:"pendingServerOptions"`
	} `json:"data"`
}

// PasswordlessLoginResponse represents the payload from PasswordlessLogin or PasswordLogin
type PasswordlessLoginResponse struct {
	Data struct {
		AuthenticationToken string `json:"authenticationToken"`
	} `json:"data"`
}

// NewAPIClient creates a new API client with self-signed TLS support
func NewAPIClient(baseURL, apiToken, serverPassword string) *APIClient {
	// Satisfactory uses a self-signed TLS certificate by default
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
	}
	return &APIClient{
		baseURL:        baseURL,
		apiToken:       apiToken,
		serverPassword: serverPassword,
		httpClient: &http.Client{
			Transport: tr,
			Timeout:   5 * time.Second,
		},
	}
}

// authenticate retrieves the configured or negotiated bearer token
func (c *APIClient) authenticate() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.apiToken != "" {
		return c.apiToken
	}
	if c.sessionToken != "" {
		return c.sessionToken
	}

	// 1. If serverPassword is provided, attempt PasswordLogin
	if c.serverPassword != "" {
		// Attempt PasswordLogin as Administrator first
		token := c.tryPasswordLogin("Administrator", c.serverPassword)
		if token == "" {
			// Fallback: try as Client if Administrator rejected
			token = c.tryPasswordLogin("Client", c.serverPassword)
		}
		if token != "" {
			c.sessionToken = token
			return token
		}
	}

	// 2. Fallback: try PasswordlessLogin with Client privilege
	reqBody := []byte(`{"function":"PasswordlessLogin","data":{"minimumPrivilegeLevel":"Client"}}`)
	req, err := http.NewRequest("POST", c.baseURL+"/api/v1", bytes.NewBuffer(reqBody))
	if err == nil {
		req.Header.Set("Content-Type", "application/json")
		if resp, err := c.httpClient.Do(req); err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var loginResp PasswordlessLoginResponse
				if err := json.NewDecoder(resp.Body).Decode(&loginResp); err == nil && loginResp.Data.AuthenticationToken != "" {
					c.sessionToken = loginResp.Data.AuthenticationToken
					return c.sessionToken
				}
			}
		}
	}

	return ""
}

func (c *APIClient) tryPasswordLogin(privilege, password string) string {
	type loginReq struct {
		Function string `json:"function"`
		Data     struct {
			MinimumPrivilegeLevel string `json:"minimumPrivilegeLevel"`
			Password              string `json:"password"`
		} `json:"data"`
	}

	var reqPayload loginReq
	reqPayload.Function = "PasswordLogin"
	reqPayload.Data.MinimumPrivilegeLevel = privilege
	reqPayload.Data.Password = password

	reqBytes, err := json.Marshal(reqPayload)
	if err != nil {
		return ""
	}

	req, err := http.NewRequest("POST", c.baseURL+"/api/v1", bytes.NewBuffer(reqBytes))
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ""
	}

	var loginResp PasswordlessLoginResponse
	if err := json.NewDecoder(resp.Body).Decode(&loginResp); err == nil && loginResp.Data.AuthenticationToken != "" {
		return loginResp.Data.AuthenticationToken
	}

	return ""
}

// CheckHealth queries the unauthenticated HealthCheck endpoint
func (c *APIClient) CheckHealth() (bool, string, error) {
	reqBody := []byte(`{"function":"HealthCheck","data":{"clientCustomData":""}}`)
	req, err := http.NewRequest("POST", c.baseURL+"/api/v1", bytes.NewBuffer(reqBody))
	if err != nil {
		return false, "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, "offline", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Sprintf("status_%d", resp.StatusCode), nil
	}

	var healthResp ServerHealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&healthResp); err != nil {
		return false, "invalid_json", err
	}

	isHealthy := healthResp.Data.Health == "healthy"
	return isHealthy, healthResp.Data.Health, nil
}

// QueryState queries QueryServerState
func (c *APIClient) QueryState() (*ServerStateResponse, error) {
	token := c.authenticate()

	reqBody := []byte(`{"function":"QueryServerState","data":{}}`)
	req, err := http.NewRequest("POST", c.baseURL+"/api/v1", bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// If unauthorized and we used a cached sessionToken, reset and try re-authenticating once
	if (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) && c.apiToken == "" {
		c.mu.Lock()
		c.sessionToken = ""
		c.mu.Unlock()
		if newToken := c.authenticate(); newToken != "" {
			reqRetry, _ := http.NewRequest("POST", c.baseURL+"/api/v1", bytes.NewBuffer(reqBody))
			reqRetry.Header.Set("Content-Type", "application/json")
			reqRetry.Header.Set("Authorization", "Bearer "+newToken)
			if respRetry, err := c.httpClient.Do(reqRetry); err == nil {
				defer respRetry.Body.Close()
				if respRetry.StatusCode == http.StatusOK {
					var stateResp ServerStateResponse
					if err := json.NewDecoder(respRetry.Body).Decode(&stateResp); err == nil {
						return &stateResp, nil
					}
				}
			}
		}
		return nil, fmt.Errorf("unauthorized: %d", resp.StatusCode)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	var stateResp ServerStateResponse
	if err := json.NewDecoder(resp.Body).Decode(&stateResp); err != nil {
		return nil, err
	}

	return &stateResp, nil
}

// QueryServerOptions queries GetServerOptions
func (c *APIClient) QueryServerOptions() (map[string]string, error) {
	token := c.authenticate()

	reqBody := []byte(`{"function":"GetServerOptions","data":{}}`)
	req, err := http.NewRequest("POST", c.baseURL+"/api/v1", bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	var optsResp ServerOptionsResponse
	if err := json.NewDecoder(resp.Body).Decode(&optsResp); err != nil {
		return nil, err
	}

	return optsResp.Data.ServerOptions, nil
}


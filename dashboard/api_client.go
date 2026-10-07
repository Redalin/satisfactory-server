package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// APIClient queries the Satisfactory 1.0 Dedicated Server HTTPS API
type APIClient struct {
	baseURL    string
	apiToken   string
	httpClient *http.Client
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
		ServerGameState struct {
			ActiveSessionName    string  `json:"activeSessionName"`
			NumConnectedPlayers  int     `json:"numConnectedPlayers"`
			PlayerLimit          int     `json:"playerLimit"`
			TechTier             int     `json:"techTier"`
			ActiveSchematic      string  `json:"activeSchematic"`
			GamePhase            string  `json:"gamePhase"`
			IsGameRunning        bool    `json:"isGameRunning"`
			TotalGameDuration    float64 `json:"totalGameDuration"`
			IsAutoSaveInProgress bool    `json:"isAutoSaveInProgress"`
		} `json:"serverGameState"`
	} `json:"data"`
}

// NewAPIClient creates a new API client with self-signed TLS support
func NewAPIClient(baseURL, apiToken string) *APIClient {
	// Satisfactory uses a self-signed TLS certificate by default
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
	}
	return &APIClient{
		baseURL:  baseURL,
		apiToken: apiToken,
		httpClient: &http.Client{
			Transport: tr,
			Timeout:   5 * time.Second,
		},
	}
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

// QueryState queries QueryServerState if a token is provided
func (c *APIClient) QueryState() (*ServerStateResponse, error) {
	if c.apiToken == "" {
		return nil, nil
	}

	reqBody := []byte(`{"function":"QueryServerState","data":{}}`)
	req, err := http.NewRequest("POST", c.baseURL+"/api/v1", bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	var stateResp ServerStateResponse
	if err := json.NewDecoder(resp.Body).Decode(&stateResp); err != nil {
		return nil, err
	}

	return &stateResp, nil
}


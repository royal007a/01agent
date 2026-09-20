package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type HTTPAgentClient struct {
	baseURL       *url.URL
	token         string
	client        *http.Client
	approvedTools []string
}

func NewHTTPAgentClient(rawURL, token string, client *http.Client, approvedTools []string) (*HTTPAgentClient, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, errors.New("agent API URL must be an absolute HTTP(S) URL")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("agent API token is required")
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &HTTPAgentClient{baseURL: parsed, token: token, client: client, approvedTools: append([]string(nil), approvedTools...)}, nil
}

func (c *HTTPAgentClient) Turn(ctx context.Context, sessionID, operationID, prompt string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"operation_id": operationID, "prompt": prompt, "approved_tools": c.approvedTools,
	})
	if err != nil {
		return "", err
	}
	endpoint := *c.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/v1/sessions/" + url.PathEscape(sessionID) + "/turns"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	limited, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("agent API returned %d: %s", response.StatusCode, strings.TrimSpace(string(limited)))
	}
	var payload struct {
		Result struct {
			FinalMessage struct {
				Content string `json:"content"`
			} `json:"final_message"`
			Reason string `json:"reason"`
		} `json:"result"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(limited, &payload); err != nil {
		return "", fmt.Errorf("decode agent response: %w", err)
	}
	if payload.Error != "" {
		return "", errors.New(payload.Error)
	}
	if strings.TrimSpace(payload.Result.FinalMessage.Content) == "" {
		return "", fmt.Errorf("agent stopped with reason %s and no final message", payload.Result.Reason)
	}
	return payload.Result.FinalMessage.Content, nil
}

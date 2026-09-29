package basispoints

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

func authHeaders(c credential, stream bool) http.Header {
	accept := "application/json"
	if stream {
		accept = "text/event-stream"
	}
	return http.Header{
		"Authorization":           []string{"Bearer " + c.AccessToken},
		"Chatgpt-Account-Id":      []string{c.AccountID},
		"X-Openai-Account-Id":     []string{c.AccountID},
		"X-Basispoints-Auth-Mode": []string{c.AuthMode},
		"Content-Type":            []string{"application/json"},
		"Accept":                  []string{accept},
		"Accept-Encoding":         []string{"identity"},
		"Origin":                  []string{"https://bps.openai.com"},
		"X-OpenAI-Internal-Basispoints-Client-Agent-Profile":  []string{"excel"},
		"X-OpenAI-Internal-Basispoints-Client-Editor":         []string{"excel"},
		"X-OpenAI-Internal-Basispoints-Client-Host":           []string{"office"},
		"X-OpenAI-Internal-Basispoints-Client-Platform":       []string{"excel"},
		"X-OpenAI-Internal-Basispoints-Client-Platform-Class": []string{"PC"},
		"X-OpenAI-Internal-Basispoints-Client-Product":        []string{"basispoints-excel-plugin"},
		"X-OpenAI-Internal-Basispoints-Client-Runtime":        []string{"desktop"},
		"X-OpenAI-Internal-Basispoints-Office-Host":           []string{"Excel"},
		"X-OpenAI-Internal-Basispoints-Office-Platform":       []string{"PC"},
		"X-Stainless-Arch":            []string{"unknown"},
		"X-Stainless-Lang":            []string{"js"},
		"X-Stainless-OS":              []string{"Unknown"},
		"X-Stainless-Package-Version": []string{"6.31.0"},
		"X-Stainless-Retry-Count":     []string{"0"},
		"X-Stainless-Runtime":         []string{"browser:chrome"},
		"User-Agent":                  []string{"oai-basispoints/" + Version},
	}
}

// Kept for the copied credential parser's non-transport compatibility paths.
// The Sub2API adapter itself never enables WebSocket routing here; the host
// owns that decision and exposes only the HTTP transport contract.
func credentialWebsocketsEnabled(request ExecutorRequest) bool {
	if raw := strings.TrimSpace(request.AuthAttributes["websockets"]); raw != "" {
		if enabled, err := strconv.ParseBool(raw); err == nil {
			return enabled
		}
	}
	value, exists := request.AuthMetadata["websockets"]
	if !exists {
		var stored map[string]any
		if json.Unmarshal(request.StorageJSON, &stored) == nil {
			value = stored["websockets"]
		}
	}
	switch flag := value.(type) {
	case bool:
		return flag
	case string:
		enabled, err := strconv.ParseBool(strings.TrimSpace(flag))
		return err == nil && enabled
	default:
		return false
	}
}

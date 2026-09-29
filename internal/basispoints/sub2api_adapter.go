package basispoints

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
)

// Sub2APIPluginID is the stable identifier used by manifest.json and GetInfo.
const Sub2APIPluginID = "oai-basispoints"

// Sub2APIPluginVersion is the version of the Sub2API transport adapter. It is
// deliberately independent from the CPA plug-in version retained in the
// reusable Basis Points protocol package.
const Sub2APIPluginVersion = "0.1.3"

const (
	defaultSub2APIResponsesURL  = DefaultResponsesURL
	defaultSub2APIUpstreamModel = DefaultUpstreamModel
	defaultSub2APITimeout       = 300
	defaultSub2APIMaxResponse   = 64 << 20
	defaultSub2APIMaxRequest    = 32 << 20
)

// Sub2APIConfig contains only non-secret plugin configuration. OAuth tokens
// are supplied by Sub2API in the outbound request headers and are never
// persisted by this adapter.
type Sub2APIConfig struct {
	ResponsesURL     string            `json:"responses_url"`
	UpstreamModel    string            `json:"upstream_model"`
	Models           []string          `json:"models"`
	ModelMappings    map[string]string `json:"model_mappings"`
	TimeoutSeconds   int               `json:"timeout_seconds"`
	MaxResponseBytes int               `json:"max_response_bytes"`
	MaxRequestBytes  int               `json:"max_request_bytes"`
	AuthMode         string            `json:"auth_mode"`
	ToolsVersionID   string            `json:"tools_version_id"`
}

// DefaultSub2APIConfig returns a complete default configuration. Returning a
// value rather than a shared pointer makes it safe for callers to mutate.
func DefaultSub2APIConfig() Sub2APIConfig {
	return Sub2APIConfig{
		ResponsesURL:     defaultSub2APIResponsesURL,
		UpstreamModel:    defaultSub2APIUpstreamModel,
		Models:           defaultModelAliases(),
		ModelMappings:    defaultModelMappings(),
		TimeoutSeconds:   defaultSub2APITimeout,
		MaxResponseBytes: defaultSub2APIMaxResponse,
		MaxRequestBytes:  defaultSub2APIMaxRequest,
		AuthMode:         "chatgpt",
	}
}

// NormalizeSub2APIConfig strictly decodes and normalizes a host-provided JSON
// configuration. The returned JSON is canonical enough for Sub2API to encrypt
// and compare as the persisted configuration.
func NormalizeSub2APIConfig(raw []byte) (Sub2APIConfig, []byte, error) {
	cfg := DefaultSub2APIConfig()
	if len(bytes.TrimSpace(raw)) > 0 {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
			return Sub2APIConfig{}, nil, fail(400, "invalid_config", "configuration must be a JSON object")
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&cfg); err != nil {
			return Sub2APIConfig{}, nil, fail(400, "invalid_config", "configuration must be a JSON object: "+err.Error())
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			return Sub2APIConfig{}, nil, fail(400, "invalid_config", "configuration must contain exactly one JSON object")
		}
		// encoding/json merges object fields into a non-nil map. Replace the
		// default mappings when the host supplied its own map, and discard the
		// default mappings when it supplied a custom model list without a map.
		if rawMappings, ok := fields["model_mappings"]; ok {
			var mappings map[string]string
			if err := json.Unmarshal(rawMappings, &mappings); err != nil {
				return Sub2APIConfig{}, nil, fail(400, "invalid_config", "model_mappings must be a JSON object")
			}
			cfg.ModelMappings = mappings
		} else if _, hasModels := fields["models"]; hasModels {
			cfg.ModelMappings = nil
		}
	}
	migrateLegacyDefaultModels(&cfg)
	core := cfg.coreConfig()
	if err := core.normalize(); err != nil {
		return Sub2APIConfig{}, nil, err
	}
	cfg = fromCoreConfig(core, cfg.MaxRequestBytes)
	if cfg.MaxRequestBytes < 64<<10 || cfg.MaxRequestBytes > 128<<20 {
		return Sub2APIConfig{}, nil, fail(400, "invalid_config", "max_request_bytes must be between 64 KiB and 128 MiB")
	}
	normalized, err := json.Marshal(cfg)
	if err != nil {
		return Sub2APIConfig{}, nil, fail(500, "invalid_config", "cannot serialize normalized configuration")
	}
	return cfg, normalized, nil
}

// migrateLegacyDefaultModels upgrades the v0.1.1/v0.1.2 default configuration
// in memory. User-defined model allowlists remain untouched; only the exact
// old default model sets are expanded so an upgrade does not keep rejecting the
// newly supported GPT-6 models until the UI is saved again.
func migrateLegacyDefaultModels(cfg *Sub2APIConfig) {
	if cfg == nil || !isLegacyDefaultModelSet(cfg.Models) {
		return
	}
	legacyMappings := map[string]string{
		DefaultModelID:    DefaultUpstreamModel,
		DefaultSolModelID: DefaultSolModelID,
	}
	for alias, upstream := range cfg.ModelMappings {
		alias = strings.TrimSpace(alias)
		expected, ok := legacyMappings[alias]
		if !ok || strings.TrimSpace(upstream) != expected {
			return
		}
	}
	existing := make(map[string]bool, len(cfg.Models))
	for _, model := range cfg.Models {
		existing[strings.TrimSpace(model)] = true
	}
	mappings := cloneStringMap(cfg.ModelMappings)
	if mappings == nil {
		mappings = make(map[string]string, len(defaultModelMappings()))
	}
	for _, model := range defaultModelAliases() {
		if !existing[model] {
			cfg.Models = append(cfg.Models, model)
		}
		mappings[model] = defaultModelMappings()[model]
	}
	cfg.ModelMappings = mappings
}

func isLegacyDefaultModelSet(models []string) bool {
	if len(models) != 1 && len(models) != 2 {
		return false
	}
	legacy := map[string]bool{DefaultModelID: true, DefaultSolModelID: true}
	seen := make(map[string]bool, len(models))
	for _, model := range models {
		model = strings.TrimSpace(model)
		if !legacy[model] || seen[model] {
			return false
		}
		seen[model] = true
	}
	return true
}

func (c Sub2APIConfig) coreConfig() Config {
	return Config{
		// The Sub2API host owns WebSocket routing. The plugin protocol is HTTP
		// only, so keep the reusable protocol's config on its HTTP path.
		UpstreamTransport:         "http",
		WSHandshakeTimeoutSeconds: 5,
		ResponsesURL:              c.ResponsesURL,
		UpstreamModel:             c.UpstreamModel,
		Models:                    append([]string(nil), c.Models...),
		ModelMappings:             cloneStringMap(c.ModelMappings),
		TimeoutSeconds:            c.TimeoutSeconds,
		MaxResponseBytes:          c.MaxResponseBytes,
		AuthMode:                  c.AuthMode,
		ToolsVersionID:            c.ToolsVersionID,
	}
}

func fromCoreConfig(core Config, maxRequestBytes int) Sub2APIConfig {
	if maxRequestBytes == 0 {
		maxRequestBytes = defaultSub2APIMaxRequest
	}
	return Sub2APIConfig{
		ResponsesURL:     core.ResponsesURL,
		UpstreamModel:    core.UpstreamModel,
		Models:           append([]string(nil), core.Models...),
		ModelMappings:    cloneStringMap(core.ModelMappings),
		TimeoutSeconds:   core.TimeoutSeconds,
		MaxResponseBytes: core.MaxResponseBytes,
		MaxRequestBytes:  maxRequestBytes,
		AuthMode:         core.AuthMode,
		ToolsVersionID:   core.ToolsVersionID,
	}
}

func cloneStringMap(input map[string]string) map[string]string {
	if input == nil {
		return nil
	}
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

// PrepareSub2APIPayload converts an OpenAI Responses request into the payload
// accepted by the Basis Points endpoint. It returns the normalized payload,
// whether the upstream response should be streamed, and any validation error.
func PrepareSub2APIPayload(raw []byte, cfg Sub2APIConfig) (map[string]any, bool, error) {
	source, err := rawObject(raw)
	if err != nil {
		return nil, false, err
	}
	if value, exists := source["stream"]; exists && value != nil {
		if _, ok := value.(bool); !ok {
			return nil, false, fail(400, "invalid_request", "stream must be a boolean")
		}
	} else {
		source["stream"] = false
	}
	if stringValue(source["model"]) == "" && len(cfg.Models) > 0 {
		source["model"] = cfg.Models[0]
	}
	prepared, err := prepareResponsesBody(source, cfg.coreConfig())
	if err != nil {
		return nil, false, err
	}
	stream, _ := prepared["stream"].(bool)
	return prepared, stream, nil
}

// MarshalSub2APIPayload serializes a payload returned by
// PrepareSub2APIPayload. Kept here so the adapter has one JSON serialization
// path and tests do not need to depend on internal helpers.
func MarshalSub2APIPayload(payload map[string]any) ([]byte, error) {
	if payload == nil {
		return nil, fail(400, "invalid_request", "prepared payload is empty")
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fail(500, "request_encoding", "cannot encode prepared Basis Points request")
	}
	return data, nil
}

// Sub2APICredential is the short-lived identity extracted from the host's
// outbound headers. It intentionally contains no refresh token.
type Sub2APICredential struct {
	AccessToken string
	AccountID   string
	AuthMode    string
}

// CredentialFromHeaders validates the OAuth header supplied by Sub2API and
// derives the ChatGPT account ID from an explicit header or JWT claims.
func CredentialFromHeaders(headers http.Header, authMode string) (Sub2APICredential, error) {
	if headers == nil {
		return Sub2APICredential{}, fail(401, "missing_auth", "Sub2API did not provide an OAuth Authorization header")
	}
	authorization := strings.TrimSpace(headers.Get("Authorization"))
	const bearerPrefix = "Bearer "
	if len(authorization) < len(bearerPrefix) || !strings.EqualFold(authorization[:len(bearerPrefix)], bearerPrefix) {
		return Sub2APICredential{}, fail(401, "missing_auth", "Sub2API did not provide a Bearer Authorization header")
	}
	token := strings.TrimSpace(authorization[len(bearerPrefix):])
	if token == "" {
		return Sub2APICredential{}, fail(401, "missing_auth", "OAuth access token is empty")
	}
	accountID := strings.TrimSpace(headers.Get("chatgpt-account-id"))
	if accountID == "" {
		accountID = strings.TrimSpace(headers.Get("x-openai-account-id"))
	}
	if accountID == "" {
		accountID = accountIDFromClaims(jwtPayload(token))
	}
	if accountID == "" {
		return Sub2APICredential{}, fail(401, "missing_account_id", "ChatGPT OAuth request has no account ID")
	}
	authMode = strings.TrimSpace(authMode)
	if authMode == "" {
		authMode = "chatgpt"
	}
	return Sub2APICredential{AccessToken: token, AccountID: accountID, AuthMode: authMode}, nil
}

// BasisPointsHeaders builds the fixed upstream header profile while replacing
// the token/account values with the host-provided OAuth identity.
func BasisPointsHeaders(headers http.Header, cfg Sub2APIConfig, stream bool) (http.Header, error) {
	identity, err := CredentialFromHeaders(headers, cfg.AuthMode)
	if err != nil {
		return nil, err
	}
	return authHeaders(credential{AccessToken: identity.AccessToken, AccountID: identity.AccountID, AuthMode: identity.AuthMode}, stream), nil
}

// ErrorInfo converts internal validation errors into the safe error frame used
// by the Sub2API transport. It never includes request bodies or credentials.
func ErrorInfo(err error) (code, message string) {
	if err == nil {
		return "", ""
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr != nil {
		code = apiErr.Code()
		message = apiErr.Message
	} else {
		code = "plugin_error"
		message = err.Error()
	}
	return sanitizeErrorCode(code), redactTokenMessage(strings.TrimSpace(message))
}

func sanitizeErrorCode(code string) string {
	code = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.' {
			return r
		}
		return -1
	}, code)
	if code == "" {
		return "plugin_error"
	}
	if len(code) > 64 {
		return code[:64]
	}
	return code
}

// Sub2APIImageUploader uploads inline data URLs to the same-origin attachments
// endpoint and replaces them with file IDs. The cache stores only a digest and
// file ID; it never retains image bytes or OAuth material.
type Sub2APIImageUploader struct {
	cache attachmentCache
}

// UploadInlineImages mutates the prepared payload in place. It is safe for
// concurrent requests because attachmentCache serializes duplicate uploads.
func (u *Sub2APIImageUploader) UploadInlineImages(ctx context.Context, client *http.Client, body map[string]any, cfg Sub2APIConfig, headers http.Header) error {
	if u == nil || client == nil || body == nil {
		return fail(500, "attachment_config", "image attachment uploader is not initialized")
	}
	identity, err := CredentialFromHeaders(headers, cfg.AuthMode)
	if err != nil {
		return err
	}
	items, _ := body["input"].([]any)
	for i, value := range items {
		item := objectValue(value)
		if stringValue(item["role"]) != "user" || stringValue(item["type"]) != "message" {
			continue
		}
		parts, _ := item["content"].([]any)
		var updated []any
		for j, value := range parts {
			part := objectValue(value)
			imageURL := stringValue(part["image_url"])
			if stringValue(part["type"]) != "input_image" || len(imageURL) < 5 || !strings.EqualFold(imageURL[:5], "data:") {
				continue
			}
			if stringValue(part["file_id"]) != "" {
				return fail(400, "invalid_image", "input_image cannot contain both image_url and file_id")
			}
			image, err := decodeInlineImage(imageURL)
			if err != nil {
				return err
			}
			endpoint, err := attachmentURL(cfg.ResponsesURL)
			if err != nil {
				return err
			}
			key := attachmentKey(endpoint, identity, image)
			fileID, err := u.cache.getOrUpload(key, func() (string, error) {
				return uploadImageHTTP(ctx, client, endpoint, image, identity, cfg.MaxResponseBytes)
			})
			if err != nil {
				return err
			}
			if updated == nil {
				updated = append([]any(nil), parts...)
			}
			copy := cloneObject(part)
			delete(copy, "image_url")
			copy["file_id"] = fileID
			if _, exists := copy["detail"]; !exists {
				copy["detail"] = "auto"
			}
			updated[j] = copy
		}
		if updated != nil {
			copy := cloneObject(item)
			copy["content"] = updated
			items[i] = copy
		}
	}
	return nil
}

func attachmentKey(endpoint string, identity Sub2APICredential, image inlineImage) [32]byte {
	hash := sha256.New()
	for _, value := range [][]byte{[]byte(endpoint), []byte(identity.AccountID), []byte(identity.AuthMode), []byte(identity.AccessToken), []byte(image.mediaType), image.data} {
		_, _ = hash.Write(value)
	}
	var key [32]byte
	copy(key[:], hash.Sum(nil))
	return key
}

func uploadImageHTTP(ctx context.Context, client *http.Client, endpoint string, image inlineImage, identity Sub2APICredential, maxResponseBytes int) (string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	filename := "image"
	if extensions, _ := mime.ExtensionsByType(image.mediaType); len(extensions) > 0 {
		filename += extensions[0]
	}
	partHeaders := make(textproto.MIMEHeader)
	partHeaders.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": filename}))
	partHeaders.Set("Content-Type", image.mediaType)
	part, err := writer.CreatePart(partHeaders)
	if err != nil {
		return "", fail(500, "attachment_encoding", "cannot encode image attachment")
	}
	if _, err := part.Write(image.data); err != nil {
		return "", fail(500, "attachment_encoding", "cannot write image attachment")
	}
	if err := writer.Close(); err != nil {
		return "", fail(500, "attachment_encoding", "cannot finish image attachment")
	}
	headers := authHeaders(credential{AccessToken: identity.AccessToken, AccountID: identity.AccountID, AuthMode: identity.AuthMode}, false)
	headers.Set("Content-Type", writer.FormDataContentType())
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body.Bytes()))
	if err != nil {
		return "", fail(500, "attachment_request", "cannot create image attachment request")
	}
	request.Header = headers
	request.ContentLength = int64(body.Len())
	response, err := client.Do(request)
	if err != nil {
		return "", fail(502, "attachment_transport", "Basis Points attachment upload transport failed")
	}
	defer response.Body.Close()
	limit := int64(maxResponseBytes)
	if limit <= 0 {
		limit = defaultSub2APIMaxResponse
	}
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if readErr != nil {
		return "", fail(502, "attachment_transport", "Basis Points attachment response could not be read")
	}
	if int64(len(raw)) > limit {
		return "", fail(502, "attachment_response_too_large", "Basis Points attachment response exceeds configured limit")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fail(response.StatusCode, "attachment_upload_error", fmt.Sprintf("Basis Points attachment upload HTTP %d: %s", response.StatusCode, attachmentErrorMessage(raw, credential{AccessToken: identity.AccessToken, AccountID: identity.AccountID}, image)))
	}
	var result struct {
		FileID string `json:"openai_file_id"`
	}
	if json.Unmarshal(raw, &result) != nil || strings.TrimSpace(result.FileID) == "" {
		return "", fail(502, "invalid_attachment_response", "Basis Points attachment upload returned no openai_file_id")
	}
	return strings.TrimSpace(result.FileID), nil
}

// ValidateResponsesURL is exposed for packager/UI tests and for callers that
// want to validate a URL without constructing a full configuration.
func ValidateResponsesURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fail(400, "invalid_config", "responses_url must be an absolute HTTP(S) URL")
	}
	return nil
}

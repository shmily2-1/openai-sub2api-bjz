package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/JaxsonWang/cpa-plugin-oai-basispoints-sub2api/internal/basispoints"
	pluginv1 "github.com/JaxsonWang/cpa-plugin-oai-basispoints-sub2api/pkg/pluginapi/v1"
	hcplugin "github.com/hashicorp/go-plugin"
	"golang.org/x/net/proxy"
	"google.golang.org/grpc"
)

const (
	capabilityID        = "openai.oauth.outbound_transport.v1"
	maxCachedTransports = 8
	ioChunkSize         = 32 * 1024
)

// Server is the Sub2API transport process. It only handles the stable HTTP
// plugin contract; Sub2API remains responsible for account selection, token
// refresh, SSE parsing, usage accounting and billing.
type Server struct {
	pluginv1.UnimplementedTransportPluginServer

	configMu sync.RWMutex
	config   basispoints.Sub2APIConfig

	clientsMu sync.Mutex
	clients   map[string]cachedClient

	hostMu          sync.RWMutex
	hostBroker      *hcplugin.GRPCBroker
	hostService     pluginv1.HostServiceClient
	hostServiceConn *grpc.ClientConn

	images basispoints.Sub2APIImageUploader
}

type cachedClient struct {
	client    *http.Client
	transport *http.Transport
	lastUsed  time.Time
}

func NewServer() *Server {
	return &Server{
		config:  basispoints.DefaultSub2APIConfig(),
		clients: make(map[string]cachedClient),
	}
}

func (s *Server) GetInfo(context.Context, *pluginv1.GetInfoRequest) (*pluginv1.GetInfoResponse, error) {
	return &pluginv1.GetInfoResponse{
		PluginId:            basispoints.Sub2APIPluginID,
		PluginVersion:       basispoints.Sub2APIPluginVersion,
		ProtocolVersion:     pluginv1.ProtocolVersion,
		TransportApiVersion: pluginv1.TransportAPIVersion,
		Capabilities:        []string{capabilityID},
	}, nil
}

func (s *Server) Health(context.Context, *pluginv1.HealthRequest) (*pluginv1.HealthResponse, error) {
	cfg := s.configSnapshot()
	status, _ := json.Marshal(map[string]any{
		"configured":        true,
		"responses_url":     cfg.ResponsesURL,
		"models":            cfg.Models,
		"timeout_seconds":   cfg.TimeoutSeconds,
		"max_request_bytes": cfg.MaxRequestBytes,
	})
	return &pluginv1.HealthResponse{
		Healthy:    true,
		Message:    "ready",
		StatusJson: string(status),
	}, nil
}

func (s *Server) ValidateConfig(_ context.Context, request *pluginv1.ValidateConfigRequest) (*pluginv1.ValidateConfigResponse, error) {
	cfg, normalized, err := basispoints.NormalizeSub2APIConfig(request.GetConfigJson())
	if err != nil {
		_, message := basispoints.ErrorInfo(err)
		return &pluginv1.ValidateConfigResponse{Valid: false, Message: message}, nil
	}
	_ = cfg
	return &pluginv1.ValidateConfigResponse{
		Valid:                true,
		Message:              "configuration is valid",
		NormalizedConfigJson: normalized,
	}, nil
}

func (s *Server) ApplyConfig(_ context.Context, request *pluginv1.ApplyConfigRequest) (*pluginv1.ApplyConfigResponse, error) {
	cfg, _, err := basispoints.NormalizeSub2APIConfig(request.GetConfigJson())
	if err != nil {
		_, message := basispoints.ErrorInfo(err)
		return &pluginv1.ApplyConfigResponse{Applied: false, Message: message}, nil
	}
	s.configMu.Lock()
	s.config = cfg
	s.configMu.Unlock()
	s.closeCachedClients()
	return &pluginv1.ApplyConfigResponse{Applied: true, Message: "configuration applied"}, nil
}

func (s *Server) TestConfig(ctx context.Context, request *pluginv1.TestConfigRequest) (*pluginv1.TestConfigResponse, error) {
	started := time.Now()
	cfg := s.configSnapshot()
	if len(bytes.TrimSpace(request.GetConfigJson())) > 0 {
		var err error
		cfg, _, err = basispoints.NormalizeSub2APIConfig(request.GetConfigJson())
		if err != nil {
			_, message := basispoints.ErrorInfo(err)
			return &pluginv1.TestConfigResponse{Success: false, Message: message, LatencyMs: time.Since(started).Milliseconds()}, nil
		}
	}
	if err := basispoints.ValidateResponsesURL(cfg.ResponsesURL); err != nil {
		_, message := basispoints.ErrorInfo(err)
		return &pluginv1.TestConfigResponse{Success: false, Message: message, LatencyMs: time.Since(started).Milliseconds()}, nil
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	status, _ := json.Marshal(map[string]any{
		"responses_url": cfg.ResponsesURL,
		"models":        cfg.Models,
		"network_probe": false,
		"message":       "configuration validated; no credential-bearing upstream probe was sent",
	})
	return &pluginv1.TestConfigResponse{
		Success:    true,
		Message:    "configuration is valid; no credential-bearing upstream probe was sent",
		LatencyMs:  time.Since(started).Milliseconds(),
		StatusJson: string(status),
	}, nil
}

// SetHostBroker receives the go-plugin broker that belongs to this plugin
// process. The broker is injected by pluginv1.GRPCPlugin before the host calls
// InitHostServices. It is intentionally kept separate from the HostService
// client because the host may restart or re-offer the reverse endpoint.
func (s *Server) SetHostBroker(broker *hcplugin.GRPCBroker) {
	s.hostMu.Lock()
	s.hostBroker = broker
	s.hostMu.Unlock()
}

// InitHostServices dials the host-provided account directory over the broker.
// OAuth credentials never arrive in ForwardRequestStart.Headers when the host
// uses the account-directory contract; they are resolved just in time from
// the selected account id during Forward.
func (s *Server) InitHostServices(ctx context.Context, request *pluginv1.InitHostServicesRequest) (*pluginv1.InitHostServicesResponse, error) {
	if request == nil || request.HostServiceId == 0 {
		return &pluginv1.InitHostServicesResponse{Ready: false, Message: "host service id is missing"}, nil
	}
	if request.HostServiceApiVersion == 0 || request.HostServiceApiVersion > pluginv1.HostServiceAPIVersion {
		return &pluginv1.InitHostServicesResponse{Ready: false, Message: "unsupported host service API version"}, nil
	}

	s.hostMu.RLock()
	broker := s.hostBroker
	s.hostMu.RUnlock()
	if broker == nil {
		return &pluginv1.InitHostServicesResponse{Ready: false, Message: "host broker is not available"}, nil
	}

	conn, err := broker.Dial(request.HostServiceId)
	if err != nil {
		return &pluginv1.InitHostServicesResponse{Ready: false, Message: "cannot dial host service"}, nil
	}
	select {
	case <-ctx.Done():
		_ = conn.Close()
		return nil, ctx.Err()
	default:
	}

	client := pluginv1.NewHostServiceClient(conn)
	s.hostMu.Lock()
	previous := s.hostServiceConn
	s.hostService = client
	s.hostServiceConn = conn
	s.hostMu.Unlock()
	if previous != nil {
		_ = previous.Close()
	}
	return &pluginv1.InitHostServicesResponse{Ready: true, Message: "host service is ready"}, nil
}

// Forward receives the host's original request and returns the raw Basis
// Points HTTP response. The handler deliberately buffers only the request,
// then streams the response body back to the host in bounded chunks.
func (s *Server) Forward(stream grpc.BidiStreamingServer[pluginv1.ForwardRequest, pluginv1.ForwardResponse]) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	start := first.GetStart()
	if start == nil {
		return sendError(stream, "invalid_request", "first plugin frame must be start", false)
	}
	if start.Method != http.MethodPost {
		return sendError(stream, "unsupported_method", "Basis Points transport accepts POST requests only", false)
	}

	cfg := s.configSnapshot()
	if start.ContentLength > int64(cfg.MaxRequestBytes) && start.ContentLength >= 0 {
		return sendError(stream, "request_too_large", "request body exceeds configured limit", false)
	}
	rawBody, err := receiveRequestBody(stream, cfg.MaxRequestBytes)
	if err != nil {
		code, message := basispoints.ErrorInfo(err)
		return sendError(stream, code, message, false)
	}

	payload, streaming, err := basispoints.PrepareSub2APIPayload(rawBody, cfg)
	if err != nil {
		code, message := basispoints.ErrorInfo(err)
		return sendError(stream, code, message, false)
	}
	requestHeaders := headersFromProto(start.Headers)
	upstreamHeaders, proxyURL, err := s.outboundHeaders(stream.Context(), start, requestHeaders, cfg, streaming)
	if err != nil {
		code, message := basispoints.ErrorInfo(err)
		return sendError(stream, code, message, false)
	}
	client, err := s.clientFor(proxyURL, cfg.TimeoutSeconds)
	if err != nil {
		code, message := basispoints.ErrorInfo(err)
		return sendError(stream, code, message, false)
	}
	if err := s.images.UploadInlineImages(stream.Context(), client, payload, cfg, upstreamHeaders); err != nil {
		code, message := basispoints.ErrorInfo(err)
		// An attachment upload may already have reached the upstream. Be
		// conservative and prevent the host from replaying the request.
		return sendError(stream, code, message, true)
	}
	encoded, err := basispoints.MarshalSub2APIPayload(payload)
	if err != nil {
		code, message := basispoints.ErrorInfo(err)
		return sendError(stream, code, message, false)
	}

	request, err := http.NewRequestWithContext(stream.Context(), http.MethodPost, cfg.ResponsesURL, bytes.NewReader(encoded))
	if err != nil {
		code, message := basispoints.ErrorInfo(err)
		return sendError(stream, code, message, false)
	}
	request.Header = upstreamHeaders
	request.ContentLength = int64(len(encoded))

	started := time.Now()
	response, err := client.Do(request)
	if err != nil {
		if stream.Context().Err() != nil {
			return stream.Context().Err()
		}
		return sendError(stream, "upstream_transport", "Basis Points upstream request failed", true)
	}
	defer response.Body.Close()
	if response.ContentLength > int64(cfg.MaxResponseBytes) {
		return sendError(stream, "upstream_response_too_large", "Basis Points response exceeds configured limit", true)
	}
	if err := sendResponseStart(stream, response); err != nil {
		return err
	}
	received, err := streamResponseBody(stream, response.Body, int64(cfg.MaxResponseBytes))
	if err != nil {
		if stream.Context().Err() != nil {
			return stream.Context().Err()
		}
		code, message := basispoints.ErrorInfo(err)
		return sendError(stream, code, message, true)
	}
	return stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_End{End: &pluginv1.ForwardResponseEnd{
		BytesReceived: received,
		DurationMs:    time.Since(started).Milliseconds(),
	}}})
}

// outboundHeaders chooses the authoritative account identity. Newer Sub2API
// hosts expose a HostService account directory and deliberately omit raw OAuth
// credentials from the forwarded request headers. Older hosts still provide
// the already-resolved headers, so the legacy path remains available when no
// HostService has been initialized.
func (s *Server) outboundHeaders(
	ctx context.Context,
	start *pluginv1.ForwardRequestStart,
	requestHeaders http.Header,
	cfg basispoints.Sub2APIConfig,
	streaming bool,
) (http.Header, string, error) {
	proxyURL := strings.TrimSpace(start.GetProxyUrl())

	identity, hostReady, err := s.resolveOutboundIdentity(ctx, start.GetAccountId())
	if err != nil {
		return nil, "", err
	}
	if !hostReady || identity == nil {
		upstream, err := basispoints.BasisPointsHeaders(requestHeaders, cfg, streaming)
		return upstream, proxyURL, err
	}

	identityHeaders := headersFromProto(identity.GetHeaders())
	if token := strings.TrimSpace(identity.GetToken()); token != "" {
		identityHeaders.Set("Authorization", "Bearer "+token)
	}
	upstream, err := basispoints.BasisPointsHeaders(identityHeaders, cfg, streaming)
	if err != nil {
		return nil, "", err
	}
	mergeHostIdentityHeaders(upstream, identityHeaders)
	// Authorization is a dedicated field in the host response. Re-assert it
	// after the header merge so a malformed host header map cannot replace it.
	upstream.Set("Authorization", "Bearer "+strings.TrimSpace(identity.GetToken()))
	return upstream, strings.TrimSpace(identity.GetProxyUrl()), nil
}

func (s *Server) resolveOutboundIdentity(ctx context.Context, accountID int64) (*pluginv1.ResolveOutboundIdentityResponse, bool, error) {
	if accountID <= 0 {
		return nil, false, nil
	}
	s.hostMu.RLock()
	client := s.hostService
	s.hostMu.RUnlock()
	if client == nil {
		return nil, false, nil
	}
	identity, err := client.ResolveOutboundIdentity(ctx, &pluginv1.ResolveOutboundIdentityRequest{AccountId: accountID})
	if err != nil {
		return nil, true, fmt.Errorf("resolve host account identity: %w", err)
	}
	if identity == nil || !identity.GetFound() {
		return nil, true, errors.New("host account identity was not found")
	}
	if identity.GetAccountId() != 0 && identity.GetAccountId() != accountID {
		return nil, true, errors.New("host account identity does not match the selected account")
	}
	platform := strings.TrimSpace(identity.GetPlatform())
	accountType := strings.TrimSpace(identity.GetAccountType())
	if (platform != "" && !strings.EqualFold(platform, "openai")) ||
		(accountType != "" && !strings.EqualFold(accountType, "oauth")) {
		return nil, true, errors.New("host account identity is not an OpenAI OAuth account")
	}
	if strings.TrimSpace(identity.GetToken()) == "" {
		return nil, true, errors.New("host account identity has no access token")
	}
	return identity, true, nil
}

// The fixed Basis Points profile owns transport and endpoint headers. Account
// identity headers from the host are copied on top, while credentials and
// client headers that belong to this adapter cannot be overridden by the
// generic host header map.
func mergeHostIdentityHeaders(target, source http.Header) {
	if target == nil {
		return
	}
	for key, values := range source {
		if !forwardHostIdentityHeader(key) {
			continue
		}
		target.Del(key)
		for _, value := range values {
			target.Add(key, value)
		}
	}
}

func forwardHostIdentityHeader(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "", "authorization", "content-type", "accept", "accept-encoding", "origin", "user-agent",
		"x-basispoints-auth-mode", "host", "cookie", "set-cookie", "proxy-authorization":
		return false
	default:
		return !isHopByHopHeader(key)
	}
}

func receiveRequestBody(stream grpc.BidiStreamingServer[pluginv1.ForwardRequest, pluginv1.ForwardResponse], limit int) ([]byte, error) {
	if limit <= 0 {
		return nil, errors.New("request body limit is not configured")
	}
	var body bytes.Buffer
	for {
		frame, err := stream.Recv()
		if err == io.EOF {
			return nil, errors.New("request stream ended before body_end")
		}
		if err != nil {
			return nil, fmt.Errorf("receive request body: %w", err)
		}
		if frame.GetStart() != nil {
			return nil, errors.New("request stream contains a second start frame")
		}
		if chunk := frame.GetBodyChunk(); len(chunk) > 0 {
			if body.Len()+len(chunk) > limit {
				return nil, errors.New("request body exceeds configured limit")
			}
			_, _ = body.Write(chunk)
		}
		if frame.GetBodyEnd() {
			return body.Bytes(), nil
		}
	}
}

func sendError(stream grpc.BidiStreamingServer[pluginv1.ForwardRequest, pluginv1.ForwardResponse], code, message string, requestSent bool) error {
	if code == "" {
		code = "plugin_error"
	}
	if message == "" {
		message = "Basis Points transport failed"
	}
	return stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_Error{Error: &pluginv1.ForwardResponseError{
		Code:        code,
		Message:     message,
		RequestSent: requestSent,
	}}})
}

func sendResponseStart(stream grpc.BidiStreamingServer[pluginv1.ForwardRequest, pluginv1.ForwardResponse], response *http.Response) error {
	protocol := response.Proto
	if protocol == "" {
		protocol = "HTTP/1.1"
	}
	status := response.Status
	if status == "" {
		status = fmt.Sprintf("%d %s", response.StatusCode, http.StatusText(response.StatusCode))
	}
	return stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_Start{Start: &pluginv1.ForwardResponseStart{
		StatusCode:    int32(response.StatusCode),
		Status:        status,
		Protocol:      protocol,
		ProtocolMajor: int32(response.ProtoMajor),
		ProtocolMinor: int32(response.ProtoMinor),
		Headers:       headersToProto(response.Header),
		ContentLength: response.ContentLength,
	}}})
}

func streamResponseBody(stream grpc.BidiStreamingServer[pluginv1.ForwardRequest, pluginv1.ForwardResponse], reader io.Reader, limit int64) (int64, error) {
	buffer := make([]byte, ioChunkSize)
	var received int64
	for {
		count, err := reader.Read(buffer)
		if count > 0 {
			if received+int64(count) > limit {
				return received, errors.New("response body exceeds configured limit")
			}
			chunk := append([]byte(nil), buffer[:count]...)
			if sendErr := stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_BodyChunk{BodyChunk: chunk}}); sendErr != nil {
				return received, sendErr
			}
			received += int64(count)
		}
		if err == io.EOF {
			return received, nil
		}
		if err != nil {
			return received, fmt.Errorf("read upstream response: %w", err)
		}
	}
}

func (s *Server) configSnapshot() basispoints.Sub2APIConfig {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	cfg := s.config
	cfg.Models = append([]string(nil), s.config.Models...)
	if s.config.ModelMappings != nil {
		cfg.ModelMappings = make(map[string]string, len(s.config.ModelMappings))
		for key, value := range s.config.ModelMappings {
			cfg.ModelMappings[key] = value
		}
	}
	return cfg
}

func (s *Server) clientFor(proxyURL string, timeoutSeconds int) (*http.Client, error) {
	key := strings.TrimSpace(proxyURL)
	s.clientsMu.Lock()
	defer s.clientsMu.Unlock()
	if cached, ok := s.clients[key]; ok {
		cached.lastUsed = time.Now()
		s.clients[key] = cached
		return cached.client, nil
	}
	transport, err := newHTTPTransport(key)
	if err != nil {
		return nil, err
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   time.Duration(timeoutSeconds) * time.Second,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) == 0 {
				return nil
			}
			previous := via[len(via)-1].URL
			if !sameOrigin(previous, request.URL) {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	if len(s.clients) >= maxCachedTransports {
		oldestKey := ""
		var oldest time.Time
		for candidateKey, candidate := range s.clients {
			if oldestKey == "" || candidate.lastUsed.Before(oldest) {
				oldestKey, oldest = candidateKey, candidate.lastUsed
			}
		}
		if oldestKey != "" {
			old := s.clients[oldestKey]
			old.transport.CloseIdleConnections()
			delete(s.clients, oldestKey)
		}
	}
	s.clients[key] = cachedClient{client: client, transport: transport, lastUsed: time.Now()}
	return client, nil
}

func (s *Server) closeCachedClients() {
	s.clientsMu.Lock()
	defer s.clientsMu.Unlock()
	for key, cached := range s.clients {
		cached.transport.CloseIdleConnections()
		delete(s.clients, key)
	}
}

func newHTTPTransport(proxyURL string) (*http.Transport, error) {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("default HTTP transport has an unsupported type")
	}
	transport := base.Clone()
	transport.Proxy = nil
	if proxyURL == "" {
		return transport, nil
	}
	u, err := url.Parse(proxyURL)
	if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("proxy_url must be an absolute proxy URL without query or fragment")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		transport.Proxy = http.ProxyURL(u)
	case "socks5", "socks5h":
		var auth *proxy.Auth
		if u.User != nil {
			password, _ := u.User.Password()
			auth = &proxy.Auth{User: u.User.Username(), Password: password}
		}
		dialer, err := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
		if err != nil {
			return nil, errors.New("cannot configure SOCKS5 proxy")
		}
		if contextDialer, ok := dialer.(proxy.ContextDialer); ok {
			transport.DialContext = contextDialer.DialContext
		} else {
			transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				return dialer.Dial(network, address)
			}
		}
	default:
		return nil, errors.New("proxy_url must use http, https, socks5, or socks5h")
	}
	return transport, nil
}

func sameOrigin(left, right *url.URL) bool {
	if left == nil || right == nil {
		return false
	}
	return strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Host, right.Host)
}

func headersFromProto(input map[string]*pluginv1.HeaderValues) http.Header {
	output := make(http.Header, len(input))
	for key, values := range input {
		if values == nil {
			continue
		}
		// Header names are case-insensitive. Canonicalize while crossing the
		// protobuf boundary so callers that supplied e.g. ChatGPT-Account-ID
		// still work with http.Header.Get("chatgpt-account-id").
		for _, value := range values.Values {
			output.Add(key, value)
		}
	}
	return output
}

func headersToProto(input http.Header) map[string]*pluginv1.HeaderValues {
	output := make(map[string]*pluginv1.HeaderValues, len(input))
	for key, values := range input {
		if isHopByHopHeader(key) {
			continue
		}
		output[key] = &pluginv1.HeaderValues{Values: append([]string(nil), values...)}
	}
	return output
}

func isHopByHopHeader(key string) bool {
	switch strings.ToLower(key) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

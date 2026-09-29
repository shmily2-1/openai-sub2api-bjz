package transport

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/JaxsonWang/cpa-plugin-oai-basispoints-sub2api/internal/basispoints"
	pluginv1 "github.com/JaxsonWang/cpa-plugin-oai-basispoints-sub2api/pkg/pluginapi/v1"
	hcplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
)

func TestHeadersFromProtoCanonicalizesCaseInsensitiveNames(t *testing.T) {
	headers := headersFromProto(map[string]*pluginv1.HeaderValues{
		"ChatGPT-Account-ID": {Values: []string{"account-1", "account-2"}},
	})
	if got := headers.Get("chatgpt-account-id"); got != "account-1" {
		t.Fatalf("headers.Get() = %q, want first account ID", got)
	}
	if got := headers.Values("CHATGPT-ACCOUNT-ID"); len(got) != 2 {
		t.Fatalf("headers.Values() = %#v, want two values", got)
	}
}

type fakeHostServiceClient struct {
	pluginv1.HostServiceClient
	response *pluginv1.ResolveOutboundIdentityResponse
	err      error
	calls    int
}

func (f *fakeHostServiceClient) ResolveOutboundIdentity(context.Context, *pluginv1.ResolveOutboundIdentityRequest, ...grpc.CallOption) (*pluginv1.ResolveOutboundIdentityResponse, error) {
	f.calls++
	return f.response, f.err
}

func TestOutboundHeadersUseHostAccountIdentity(t *testing.T) {
	server := NewServer()
	server.hostService = &fakeHostServiceClient{response: &pluginv1.ResolveOutboundIdentityResponse{
		Found:       true,
		AccountId:   42,
		Platform:    "openai",
		AccountType: "oauth",
		ProxyUrl:    "http://account-proxy.invalid:8080",
		Token:       "host-access-token",
		Headers: map[string]*pluginv1.HeaderValues{
			"ChatGPT-Account-ID": {Values: []string{"host-chatgpt-account"}},
			"X-OpenAI-FedRamp":   {Values: []string{"true"}},
		},
	}}

	requestHeaders := http.Header{
		"Authorization":       []string{"Bearer client-token"},
		"ChatGPT-Account-ID":  []string{"client-chatgpt-account"},
		"X-OpenAI-Account-ID": []string{"client-account"},
	}
	start := &pluginv1.ForwardRequestStart{AccountId: 42, ProxyUrl: "http://request-proxy.invalid:8080"}
	upstream, proxyURL, err := server.outboundHeaders(context.Background(), start, requestHeaders, basispointsConfigForTest(), true)
	if err != nil {
		t.Fatal(err)
	}
	if got := upstream.Get("Authorization"); got != "Bearer host-access-token" {
		t.Fatalf("Authorization = %q, want host identity", got)
	}
	if got := upstream.Get("ChatGPT-Account-ID"); got != "host-chatgpt-account" {
		t.Fatalf("ChatGPT-Account-ID = %q, want host identity", got)
	}
	if got := upstream.Get("X-OpenAI-Account-ID"); got != "host-chatgpt-account" {
		t.Fatalf("X-OpenAI-Account-ID = %q, want fixed identity derived from host account", got)
	}
	if got := upstream.Get("X-OpenAI-FedRamp"); got != "true" {
		t.Fatalf("X-OpenAI-FedRamp = %q, want host header", got)
	}
	if proxyURL != "http://account-proxy.invalid:8080" {
		t.Fatalf("proxyURL = %q, want account proxy", proxyURL)
	}
	if server.hostService.(*fakeHostServiceClient).calls != 1 {
		t.Fatal("host account identity was not resolved exactly once")
	}
}

func TestOutboundHeadersFallBackToForwardedHeadersWithoutHostService(t *testing.T) {
	server := NewServer()
	requestHeaders := make(http.Header)
	requestHeaders.Set("Authorization", "Bearer forwarded-token")
	requestHeaders.Set("ChatGPT-Account-ID", "forwarded-account")
	start := &pluginv1.ForwardRequestStart{AccountId: 42, ProxyUrl: "http://request-proxy.invalid:8080"}
	upstream, proxyURL, err := server.outboundHeaders(context.Background(), start, requestHeaders, basispointsConfigForTest(), false)
	if err != nil {
		t.Fatal(err)
	}
	if got := upstream.Get("Authorization"); got != "Bearer forwarded-token" {
		t.Fatalf("Authorization = %q, want forwarded identity", got)
	}
	if proxyURL != "http://request-proxy.invalid:8080" {
		t.Fatalf("proxyURL = %q, want forwarded proxy", proxyURL)
	}
}

func TestOutboundHeadersDoNotFallBackWhenHostResolutionFails(t *testing.T) {
	server := NewServer()
	server.hostService = &fakeHostServiceClient{err: errors.New("host unavailable")}
	requestHeaders := make(http.Header)
	requestHeaders.Set("Authorization", "Bearer forwarded-token")
	requestHeaders.Set("ChatGPT-Account-ID", "forwarded-account")
	_, _, err := server.outboundHeaders(context.Background(), &pluginv1.ForwardRequestStart{AccountId: 42}, requestHeaders, basispointsConfigForTest(), false)
	if err == nil {
		t.Fatal("expected host resolution failure")
	}
}

func basispointsConfigForTest() basispoints.Sub2APIConfig {
	return basispoints.DefaultSub2APIConfig()
}

type hostServiceTestServer struct {
	pluginv1.UnimplementedHostServiceServer
	response *pluginv1.ResolveOutboundIdentityResponse
}

func (s *hostServiceTestServer) ResolveOutboundIdentity(context.Context, *pluginv1.ResolveOutboundIdentityRequest) (*pluginv1.ResolveOutboundIdentityResponse, error) {
	return s.response, nil
}

func TestInitHostServicesDialsBrokerAndResolvesAccount(t *testing.T) {
	server := NewServer()
	client, _ := hcplugin.TestPluginGRPCConn(t, false, map[string]hcplugin.Plugin{
		pluginv1.TransportPluginName: &pluginv1.GRPCPlugin{Impl: server},
	})
	t.Cleanup(func() { _ = client.Close() })

	dispensed, err := client.Dispense(pluginv1.TransportPluginName)
	if err != nil {
		t.Fatal(err)
	}
	transportClient, ok := dispensed.(*pluginv1.TransportClient)
	if !ok || transportClient.Broker == nil {
		t.Fatal("transport client did not expose a broker")
	}

	host := &hostServiceTestServer{response: &pluginv1.ResolveOutboundIdentityResponse{
		Found:       true,
		AccountId:   7,
		Platform:    "openai",
		AccountType: "oauth",
		Token:       "broker-token",
		Headers: map[string]*pluginv1.HeaderValues{
			"ChatGPT-Account-ID": {Values: []string{"broker-account"}},
		},
	}}
	brokerID := transportClient.Broker.NextId()
	go transportClient.Broker.AcceptAndServe(brokerID, func(opts []grpc.ServerOption) *grpc.Server {
		grpcServer := grpc.NewServer(opts...)
		pluginv1.RegisterHostServiceServer(grpcServer, host)
		return grpcServer
	})

	initResponse, err := transportClient.InitHostServices(context.Background(), &pluginv1.InitHostServicesRequest{
		HostServiceId:         brokerID,
		HostServiceApiVersion: pluginv1.HostServiceAPIVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if initResponse == nil || !initResponse.Ready {
		t.Fatalf("unexpected InitHostServices response: %#v", initResponse)
	}
	identity, ready, err := server.resolveOutboundIdentity(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if !ready || identity == nil || identity.GetToken() != "broker-token" {
		t.Fatalf("unexpected broker identity: ready=%v identity=%#v", ready, identity)
	}
}

func TestForwardUsesResolvedHostIdentityForUpstream(t *testing.T) {
	var receivedAuth, receivedAccount string
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		receivedAuth = request.Header.Get("Authorization")
		receivedAccount = request.Header.Get("ChatGPT-Account-ID")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"id":"resp_test","output":[]}`)
	}))
	defer upstream.Close()

	server := NewServer()
	server.hostService = &fakeHostServiceClient{response: &pluginv1.ResolveOutboundIdentityResponse{
		Found:       true,
		AccountId:   42,
		Platform:    "openai",
		AccountType: "oauth",
		Token:       "resolved-token",
		Headers: map[string]*pluginv1.HeaderValues{
			"ChatGPT-Account-ID": {Values: []string{"resolved-account"}},
		},
	}}
	config := basispoints.DefaultSub2APIConfig()
	config.ResponsesURL = upstream.URL
	server.config = config

	client, _ := hcplugin.TestPluginGRPCConn(t, false, map[string]hcplugin.Plugin{
		pluginv1.TransportPluginName: &pluginv1.GRPCPlugin{Impl: server},
	})
	t.Cleanup(func() { _ = client.Close() })
	dispensed, err := client.Dispense(pluginv1.TransportPluginName)
	if err != nil {
		t.Fatal(err)
	}
	transportClient := dispensed.(*pluginv1.TransportClient)
	stream, err := transportClient.Forward(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"model":"gpt-6-astra-basispoints","stream":false,"input":[{"role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)
	if err := stream.Send(&pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_Start{Start: &pluginv1.ForwardRequestStart{
		Method:        http.MethodPost,
		AccountId:     42,
		Platform:      "openai",
		AccountType:   "oauth",
		ContentLength: int64(len(body)),
		HasBody:       true,
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_BodyChunk{BodyChunk: body}}); err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_BodyEnd{BodyEnd: true}}); err != nil {
		t.Fatal(err)
	}
	first, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if first.GetError() != nil {
		t.Fatalf("unexpected plugin error: %#v", first.GetError())
	}
	if first.GetStart() == nil || first.GetStart().GetStatusCode() != http.StatusOK {
		t.Fatalf("unexpected response start: %#v", first)
	}
	var responseBody bytes.Buffer
	for {
		frame, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if chunk := frame.GetBodyChunk(); len(chunk) > 0 {
			_, _ = responseBody.Write(chunk)
		}
		if frame.GetEnd() != nil {
			break
		}
	}
	if receivedAuth != "Bearer resolved-token" || receivedAccount != "resolved-account" {
		t.Fatalf("upstream received auth=%q account=%q, want resolved host identity", receivedAuth, receivedAccount)
	}
	if !bytes.Contains(responseBody.Bytes(), []byte(`resp_test`)) {
		t.Fatalf("unexpected response body: %s", responseBody.Bytes())
	}
}

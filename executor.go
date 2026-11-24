package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type executorRequest struct {
	Model, Alt      string
	Headers         http.Header
	OriginalRequest []byte
	Metadata        map[string]any
	StreamID        string `json:"stream_id"`
	HostCallbackID  string `json:"host_callback_id"`
}

type upstreamRequest struct {
	Method         string      `json:"method"`
	URL            string      `json:"url"`
	Headers        http.Header `json:"headers"`
	Body           []byte      `json:"body"`
	HostCallbackID string      `json:"host_callback_id"`
}

type upstreamResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

func (p *plugin) execute(req executorRequest, stream bool) (any, error) {
	upstream, scope, err := p.prepare(req, stream)
	if err != nil {
		return nil, err
	}
	if stream {
		return p.executeStream(req.StreamID, upstream, scope)
	}

	var resp upstreamResponse
	if err := p.host("host.http.do", upstream, &resp); err != nil {
		return nil, upstreamUnavailable()
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, upstreamError(resp.StatusCode, resp.Body, upstream.Headers.Get("Authorization"))
	}
	if !json.Valid(resp.Body) {
		return nil, rejected(502, "invalid_upstream_response", "upstream returned invalid Responses JSON")
	}
	if err := p.record(scope, gjson.GetBytes(resp.Body, "output")); err != nil {
		return nil, provenanceUnavailable()
	}
	if req.Alt == "" {
		if err := p.recordResponseID(scope, gjson.ParseBytes(resp.Body)); err != nil {
			return nil, provenanceUnavailable()
		}
	}
	return map[string]any{"Payload": resp.Body, "Headers": responseHeaders(resp.Headers)}, nil
}

func (p *plugin) prepare(req executorRequest, stream bool) (upstreamRequest, provenanceScope, error) {
	path, _ := req.Metadata["request_path"].(string)
	if (path != "/v1/responses" && path != "/v1/responses/compact") || isCodex(req.Headers) {
		return upstreamRequest{}, provenanceScope{}, rejected(400, "unsupported_client", "this provider accepts OpenAI Responses requests from non-Codex clients")
	}
	endpoint := "/responses"
	switch {
	case path == "/v1/responses/compact" && req.Alt == "responses/compact" && !stream:
		endpoint += "/compact"
	case path == "/v1/responses" && req.Alt == "":
	default:
		return upstreamRequest{}, provenanceScope{}, rejected(400, "unsupported_operation", "unsupported Responses operation")
	}
	p.mu.RLock()
	cfg := p.config
	p.mu.RUnlock()
	if cfg.apiKey == "" {
		return upstreamRequest{}, provenanceScope{}, rejected(503, "provider_unconfigured", "the Responses provider has no project API key")
	}
	model, prefixed := strings.CutPrefix(req.Model, provider+"/")
	if !prefixed || !p.catalog.contains(cfg, model) {
		return upstreamRequest{}, provenanceScope{}, rejected(404, "model_not_found", "model is not exposed by the Responses provider")
	}
	root := gjson.ParseBytes(req.OriginalRequest)
	if !json.Valid(req.OriginalRequest) || !root.IsObject() {
		return upstreamRequest{}, provenanceScope{}, rejected(400, "invalid_request", "expected a Responses JSON object")
	}
	opaque, valid := requestOpaque(root)
	if !valid {
		return upstreamRequest{}, provenanceScope{}, rejected(400, "invalid_request", "duplicate or unsupported fields in Responses input")
	}
	caller, _ := req.Metadata["caller_scope"].(string)
	scope := provenanceScope{namespace: cfg.namespace, caller: caller}
	if err := p.verifyResponseID(scope, root.Get("previous_response_id")); err != nil {
		return upstreamRequest{}, provenanceScope{}, err
	}
	if len(opaque) != 0 {
		if err := p.verify(scope, opaque); err != nil {
			return upstreamRequest{}, provenanceScope{}, err
		}
	}
	body, err := routeBody(req.OriginalRequest, root, model, endpoint, stream)
	if err != nil {
		return upstreamRequest{}, provenanceScope{}, err
	}

	headers := http.Header{
		"Authorization": {"Bearer " + cfg.apiKey},
		"Content-Type":  {"application/json"},
		"User-Agent":    {"nothing-gateway-openai-responses/" + version},
	}
	if stream {
		headers.Set("Accept", "text/event-stream")
	} else {
		headers.Set("Accept", "application/json")
	}
	return upstreamRequest{
		Method: http.MethodPost, URL: cfg.BaseURL + endpoint, Headers: headers, Body: body,
		HostCallbackID: req.HostCallbackID,
	}, scope, nil
}

func routeBody(body []byte, root gjson.Result, model, endpoint string, stream bool) ([]byte, error) {
	// Avoid reserializing input or tools, and preserve cache keys, retention,
	// and breakpoints verbatim.
	var err error
	if field := root.Get("model"); field.Type != gjson.String || field.String() != model {
		body, err = sjson.SetBytes(body, "model", model)
		if err != nil {
			return nil, err
		}
	}
	field := root.Get("stream")
	if endpoint == "/responses" && ((stream && field.Type != gjson.True) ||
		(!stream && field.Exists() && field.Type != gjson.False)) {
		return sjson.SetBytes(body, "stream", stream)
	}
	return body, nil
}

func isCodex(headers http.Header) bool {
	for name, values := range headers {
		switch strings.ToLower(name) {
		case "user-agent", "originator":
			for _, value := range values {
				if strings.Contains(strings.ToLower(value), "codex") {
					return true
				}
			}
		default:
			if strings.HasPrefix(strings.ToLower(name), "x-codex-") {
				return true
			}
		}
	}
	return false
}

func upstreamUnavailable() error {
	return rejected(502, "upstream_unavailable", "Responses upstream transport failed")
}

func upstreamError(status int, body []byte, authorization string) error {
	if status < 400 || status > 599 {
		return rejected(502, "unexpected_upstream_status", "Responses upstream returned an unexpected HTTP status")
	}
	message := http.StatusText(status)
	// The host recognizes JSON error messages. Avoid leaking echoed credentials.
	key := strings.TrimPrefix(authorization, "Bearer ")
	if len(body) <= maxErrorBody && json.Valid(body) && gjson.GetBytes(body, "error").IsObject() &&
		(key == "" || !bytes.Contains(body, []byte(key))) {
		message = string(body)
	}
	code := gjson.Get(message, "error.code").String()
	if code == "" {
		code = "upstream_error"
	}
	return rejected(status, code, message)
}

func responseHeaders(headers http.Header) http.Header {
	result := headers.Clone()
	for name := range result {
		switch strings.ToLower(name) {
		case "connection", "content-length", "content-encoding", "transfer-encoding", "set-cookie":
			delete(result, name)
		}
	}
	return result
}

const maxErrorBody = 64 << 10

var errTruncatedStream = errors.New("Responses upstream closed before a terminal event")

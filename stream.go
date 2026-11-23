package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

const maxEventBytes = 16 << 20

type streamID struct {
	ID string `json:"stream_id"`
}

type streamReader struct {
	plugin  *plugin
	id      streamID
	pending []byte
	end     error
}

func (r *streamReader) Read(dst []byte) (int, error) {
	for len(r.pending) == 0 && r.end == nil {
		var chunk struct {
			Payload []byte `json:"payload"`
			Error   string `json:"error"`
			Done    bool   `json:"done"`
		}
		if err := r.plugin.host("host.http.stream_read", r.id, &chunk); err != nil {
			r.end = upstreamUnavailable()
			break
		}
		r.pending = chunk.Payload
		if chunk.Error != "" {
			r.end = upstreamUnavailable()
		} else if chunk.Done {
			r.end = io.EOF
		}
	}
	if len(r.pending) == 0 {
		return 0, r.end
	}
	n := copy(dst, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func (p *plugin) executeStream(id string, req upstreamRequest, scope provenanceScope) (any, error) {
	if id == "" {
		return nil, upstreamUnavailable()
	}
	var opened struct {
		StatusCode int         `json:"status_code"`
		Headers    http.Header `json:"headers"`
		StreamID   string      `json:"stream_id"`
	}
	if err := p.host("host.http.do_stream", req, &opened); err != nil || opened.StreamID == "" {
		return nil, upstreamUnavailable()
	}
	reader := &streamReader{plugin: p, id: streamID{ID: opened.StreamID}}
	if opened.StatusCode < 200 || opened.StatusCode >= 300 {
		defer p.host("host.http.stream_close", reader.id, nil)
		body, _ := io.ReadAll(io.LimitReader(reader, maxErrorBody+1))
		return nil, upstreamError(opened.StatusCode, body, req.Headers.Get("Authorization"))
	}
	if contentType := opened.Headers.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(contentType), "text/event-stream") {
		_ = p.host("host.http.stream_close", reader.id, nil)
		return nil, rejected(502, "invalid_upstream_response", "upstream did not return a Responses event stream")
	}
	go func() {
		err := p.relayResponses(reader, streamID{ID: id}, scope)
		_ = p.host("host.http.stream_close", reader.id, nil)
		closeReq := struct {
			streamID
			Error string `json:"error,omitempty"`
		}{streamID: streamID{ID: id}}
		if err != nil {
			closeReq.Error = err.Error()
		}
		_ = p.host("host.stream.close", closeReq, nil)
	}()
	return map[string]any{"headers": responseHeaders(opened.Headers)}, nil
}

func (p *plugin) relayResponses(reader io.Reader, id streamID, scope provenanceScope) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 32<<10), maxEventBytes)
	var frame, data []byte
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(frame)+len(line)+1 > maxEventBytes {
			return errors.New("Responses event exceeds the stream buffer limit")
		}
		frame = append(frame, line...)
		frame = append(frame, '\n')
		if payload, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			payload = bytes.TrimPrefix(payload, []byte(" "))
			data = append(data, payload...)
			data = append(data, '\n')
		}
		if len(line) != 0 {
			continue
		}
		terminal, err := p.emitFrame(id, scope, frame, bytes.TrimSpace(data))
		if terminal || err != nil {
			return err
		}
		frame, data = frame[:0], data[:0]
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return errTruncatedStream
}

func (p *plugin) emitFrame(id streamID, scope provenanceScope, frame, data []byte) (bool, error) {
	if len(data) == 0 {
		return false, nil
	}
	if bytes.Equal(data, []byte("[DONE]")) {
		return false, errTruncatedStream
	}
	if !json.Valid(data) {
		return false, errors.New("upstream returned an invalid Responses event")
	}

	var terminal bool
	event := gjson.ParseBytes(data)
	var items gjson.Result
	switch event.Get("type").String() {
	case "response.output_item.done":
		items = event.Get("item")
	case "response.completed", "response.incomplete", "response.failed", "response.cancelled", "error":
		terminal = true
		items = event.Get("response.output")
	}
	if err := p.record(scope, items); err != nil {
		return false, provenanceUnavailable()
	}
	err := p.host("host.stream.emit", struct {
		streamID
		Payload []byte `json:"payload"`
	}{streamID: id, Payload: frame}, nil)
	if err != nil {
		return false, errors.New("Responses stream delivery stopped")
	}
	return terminal, nil
}

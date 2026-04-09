package mcp

import (
	"strings"
	"testing"
)

func TestDecodeSSEResponse_plainData(t *testing.T) {
	body := "data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n"
	r, err := decodeSSEResponse(strings.NewReader(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.JSONRPC != "2.0" {
		t.Errorf("JSONRPC = %q, want 2.0", r.JSONRPC)
	}
}

func TestDecodeSSEResponse_spacesAfterColon(t *testing.T) {
	// SSE spec allows "data:payload" or "data: payload" — both must work.
	body := "data:   {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{}}\n\n"
	_, err := decodeSSEResponse(strings.NewReader(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDecodeSSEResponse_skipsEventAndCommentLines(t *testing.T) {
	body := ": keep-alive\n" +
		"event: message\n" +
		"data: {\"jsonrpc\":\"2.0\",\"id\":3,\"result\":{}}\n\n"
	_, err := decodeSSEResponse(strings.NewReader(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDecodeSSEResponse_returnsFirstDataEvent(t *testing.T) {
	// Only the first data: line should be decoded; subsequent ones are ignored.
	body := "data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n" +
		"data: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{}}\n\n"
	r, err := decodeSSEResponse(strings.NewReader(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := int(r.ID.(float64)); got != 1 {
		t.Errorf("ID = %d, want 1 (first event)", got)
	}
}

func TestDecodeSSEResponse_emptyBody(t *testing.T) {
	_, err := decodeSSEResponse(strings.NewReader(""))
	if err == nil {
		t.Fatal("expected error for empty body, got nil")
	}
}

func TestDecodeSSEResponse_noDataLine(t *testing.T) {
	body := "event: message\n: comment\n\n"
	_, err := decodeSSEResponse(strings.NewReader(body))
	if err == nil {
		t.Fatal("expected error when no data: line present, got nil")
	}
}

func TestDecodeSSEResponse_invalidJSON(t *testing.T) {
	body := "data: not-json\n\n"
	_, err := decodeSSEResponse(strings.NewReader(body))
	if err == nil {
		t.Fatal("expected error for invalid JSON payload, got nil")
	}
}

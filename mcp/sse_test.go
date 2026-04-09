package mcp

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"strings"
	"testing"
	"time"
)

// pipeResponse returns an http.Response whose body is backed by an io.Pipe,
// so the caller controls exactly when the stream closes.
func pipeResponse() (*http.Response, *io.PipeWriter) {
	pr, pw := io.Pipe()
	resp := &http.Response{Body: io.NopCloser(pr)}
	return resp, pw
}

func TestSSEConn_readLoop_logsDisconnectOnServerClose(t *testing.T) {
	resp, pw := pipeResponse()

	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(nil) })

	conn := newSSEConn(context.Background())
	go conn.readLoop(resp, "http://test.example/sse")

	// Closing the writer simulates the server closing the SSE stream.
	_ = pw.Close()

	select {
	case <-conn.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("readLoop did not cancel context after server disconnect")
	}
	time.Sleep(10 * time.Millisecond) // let deferred log flush

	if !strings.Contains(buf.String(), "disconnected") {
		t.Errorf("expected disconnect log after server close, got: %q", buf.String())
	}
}

func TestSSEConn_readLoop_noLogOnIntentionalCancel(t *testing.T) {
	// When the client intentionally cancels first, readLoop should NOT log a disconnect.
	resp, pw := pipeResponse()
	defer func() { _ = pw.Close() }()

	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(nil) })

	conn := newSSEConn(context.Background())
	go conn.readLoop(resp, "http://test.example/sse")

	// Cancel the context first — intentional client-side close.
	conn.cancel()
	// Unblock the scanner so readLoop can actually exit.
	_ = pw.Close()

	select {
	case <-conn.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("readLoop did not exit after context cancel")
	}
	time.Sleep(10 * time.Millisecond)

	if strings.Contains(buf.String(), "disconnected") {
		t.Errorf("unexpected disconnect log on intentional cancel: %q", buf.String())
	}
}

func TestSSEConn_readLoop_logsScanError(t *testing.T) {
	// Simulate a scan error (pipe closed with an error).
	pr, pw := io.Pipe()
	resp := &http.Response{Body: io.NopCloser(pr)}

	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(nil) })

	conn := newSSEConn(context.Background())
	go conn.readLoop(resp, "http://test.example/sse")

	pw.CloseWithError(io.ErrUnexpectedEOF)

	select {
	case <-conn.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("readLoop did not exit after scan error")
	}
	time.Sleep(10 * time.Millisecond)

	got := buf.String()
	if !strings.Contains(got, "disconnected") {
		t.Errorf("expected disconnect log with error, got: %q", got)
	}
	if !strings.Contains(got, "unexpected EOF") {
		t.Errorf("expected error text in disconnect log, got: %q", got)
	}
}

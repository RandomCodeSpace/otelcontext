package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"
)

func TestStartPprofServerDisabledOnEmptyAddr(t *testing.T) {
	srv, addr, err := startPprofServer("", slog.Default())
	if err != nil || srv != nil || addr != nil {
		t.Fatalf("empty addr must disable pprof: srv=%v addr=%v err=%v", srv, addr, err)
	}
}

func TestStartPprofServerServesHeapProfile(t *testing.T) {
	srv, addr, err := startPprofServer("127.0.0.1:0", slog.Default())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://%s/debug/pprof/heap?debug=1", addr))
	if err != nil {
		t.Fatalf("get heap profile: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%.200s", resp.StatusCode, body)
	}
	if len(body) == 0 {
		t.Fatal("empty heap profile body")
	}
}

func TestStartPprofServerInvalidAddr(t *testing.T) {
	srv, _, err := startPprofServer("definitely-not-an-addr", slog.Default())
	if err == nil {
		srv.Close()
		t.Fatal("expected listen error for invalid addr")
	}
}

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestHostID(t *testing.T) {
	t.Run("env var set", func(t *testing.T) {
		t.Setenv("FASTDEDUP_HOST_ID", "myhost")
		if got := hostID(); got != "myhost" {
			t.Errorf("hostID() = %q, want %q", got, "myhost")
		}
	})

	t.Run("env var empty", func(t *testing.T) {
		t.Setenv("FASTDEDUP_HOST_ID", "")
		got := hostID()
		hostname, _ := os.Hostname()
		if hostname != "" && got != hostname {
			t.Errorf("hostID() = %q, want hostname %q", got, hostname)
		}
	})

	t.Run("fallback to hostname", func(t *testing.T) {
		// Don't set the env var — use default.
		got := hostID()
		if got == "" {
			t.Error("hostID() should not be empty")
		}
	})
}

func TestSendWebhook(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		var receivedBody string
		var receivedContentType string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			receivedContentType = r.Header.Get("Content-Type")
			body, _ := io.ReadAll(r.Body)
			receivedBody = string(body)
			w.WriteHeader(200)
		}))
		defer srv.Close()

		err := sendWebhook(srv.URL, "hello world")
		if err != nil {
			t.Fatal(err)
		}
		if receivedContentType != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", receivedContentType)
		}
		var payload webhookPayload
		json.Unmarshal([]byte(receivedBody), &payload)
		if payload.Text != "hello world" {
			t.Errorf("text = %q, want %q", payload.Text, "hello world")
		}
	})

	t.Run("server error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(500)
		}))
		defer srv.Close()

		err := sendWebhook(srv.URL, "test")
		if err == nil {
			t.Error("expected error for 500 response")
		}
		if !strings.Contains(err.Error(), "500") {
			t.Errorf("error should mention status code: %v", err)
		}
	})

	t.Run("400 error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(400)
		}))
		defer srv.Close()

		err := sendWebhook(srv.URL, "test")
		if err == nil {
			t.Error("expected error for 400 response")
		}
	})

	t.Run("299 success boundary", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(299)
		}))
		defer srv.Close()

		err := sendWebhook(srv.URL, "test")
		if err != nil {
			t.Errorf("299 should succeed, got: %v", err)
		}
	})

	t.Run("connection refused", func(t *testing.T) {
		err := sendWebhook("http://127.0.0.1:1", "test")
		if err == nil {
			t.Error("expected error for connection refused")
		}
	})
}

func TestNotifyUpdate(t *testing.T) {
	t.Setenv("FASTDEDUP_HOST_ID", "test-host")

	t.Run("with deduped files", func(t *testing.T) {
		var received string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			var p webhookPayload
			json.Unmarshal(body, &p)
			received = p.Text
			w.WriteHeader(200)
		}))
		defer srv.Close()

		stats := &DedupStats{FilesDeduped: 10, BytesSaved: 1048576, AlreadyDeduped: 5, Errors: 2}
		notifyUpdate(srv.URL, "/mnt/data", stats, 3*time.Second, false)

		if !strings.Contains(received, "test-host") {
			t.Error("message should contain host ID")
		}
		if !strings.Contains(received, "/mnt/data") {
			t.Error("message should contain root path")
		}
		if !strings.Contains(received, "10") {
			t.Error("message should contain deduped count")
		}
	})

	t.Run("no changes", func(t *testing.T) {
		var received string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			var p webhookPayload
			json.Unmarshal(body, &p)
			received = p.Text
			w.WriteHeader(200)
		}))
		defer srv.Close()

		stats := &DedupStats{AlreadyDeduped: 100}
		notifyUpdate(srv.URL, "/mnt/data", stats, time.Second, false)

		if !strings.Contains(received, "No changes") {
			t.Error("message should say 'No changes'")
		}
	})

	t.Run("dry run prefix", func(t *testing.T) {
		var received string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			var p webhookPayload
			json.Unmarshal(body, &p)
			received = p.Text
			w.WriteHeader(200)
		}))
		defer srv.Close()

		stats := &DedupStats{FilesDeduped: 1, BytesSaved: 100}
		notifyUpdate(srv.URL, "/test", stats, time.Second, true)

		if !strings.HasPrefix(received, "[dry-run]") {
			t.Errorf("dry-run message should start with [dry-run], got: %q", received)
		}
	})
}

func TestNotifyAlert(t *testing.T) {
	t.Setenv("FASTDEDUP_HOST_ID", "alert-host")

	var received string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p webhookPayload
		json.Unmarshal(body, &p)
		received = p.Text
		w.WriteHeader(200)
	}))
	defer srv.Close()

	stats := &DedupStats{Errors: 5}
	notifyAlert(srv.URL, "/mnt/storage", stats)

	if !strings.Contains(received, "alert-host") {
		t.Error("alert should contain host ID")
	}
	if !strings.Contains(received, "/mnt/storage") {
		t.Error("alert should contain root path")
	}
	if !strings.Contains(received, "5") {
		t.Error("alert should contain error count")
	}
	if !strings.Contains(received, "investigation") {
		t.Error("alert should mention investigation")
	}
}

func TestPingHealthcheck(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		pinged := false
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				t.Errorf("expected GET, got %s", r.Method)
			}
			pinged = true
			w.WriteHeader(200)
		}))
		defer srv.Close()

		pingHealthcheck(srv.URL)
		if !pinged {
			t.Error("healthcheck was not pinged")
		}
	})

	t.Run("server error no panic", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(500)
		}))
		defer srv.Close()

		// Should not panic.
		pingHealthcheck(srv.URL)
	})

	t.Run("connection failure no panic", func(t *testing.T) {
		pingHealthcheck("http://127.0.0.1:1")
	})
}

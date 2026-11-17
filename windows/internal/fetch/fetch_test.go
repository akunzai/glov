package fetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDownloadEndsWhenTheServerStopsSending(t *testing.T) {
	old := IdleTimeout
	IdleTimeout = 200 * time.Millisecond
	t.Cleanup(func() { IdleTimeout = old })
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			time.Sleep(50 * time.Millisecond)
			w.Write([]byte("a"))
			w.(http.Flusher).Flush()
			time.Sleep(100 * time.Millisecond) // under the limit: still alive
			w.Write([]byte("b"))
			return
		}
		w.Header().Set("Content-Length", "100")
		w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	path := filepath.Join(t.TempDir(), "img")
	start := time.Now()
	err := Download(context.Background(), srv.Client(), srv.URL+"/stall", path, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "no data for") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("took %s", time.Since(start))
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("the partial download was kept")
	}
	if b, err := Get(context.Background(), srv.Client(), srv.URL+"/slow", 10); err != nil || string(b) != "ab" {
		t.Fatalf("slow Get = %q, %v", b, err)
	}
}

package sandock_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sandock/sandock/sdk/go/sandock"
)

// newTestServer creates a minimal httptest.Server that handles Sandock API routes.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			sb := sandock.Sandbox{
				ID:        "sb-test001",
				TenantID:  "tenant-abc",
				State:     "provisioning",
				CreatedAt: time.Now(),
				UpdatedAt: time.Now(),
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(sb)
		case http.MethodGet:
			sbs := []*sandock.Sandbox{{
				ID: "sb-test001", State: "running",
				CreatedAt: time.Now(), UpdatedAt: time.Now(),
			}}
			json.NewEncoder(w).Encode(sbs)
		}
	})

	mux.HandleFunc("/v1/sandboxes/sb-test001", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			sb := sandock.Sandbox{ID: "sb-test001", State: "running"}
			json.NewEncoder(w).Encode(sb)
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		}
	})

	mux.HandleFunc("/v1/sandboxes/sb-test001/exec", func(w http.ResponseWriter, r *http.Request) {
		result := sandock.ExecResult{
			Stdout:     "hello\n",
			ExitCode:   0,
			DurationMs: 5,
		}
		json.NewEncoder(w).Encode(result)
	})

	return httptest.NewServer(mux)
}

func TestCreate(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	client := sandock.NewClient(srv.URL, "test-key")
	sb, err := client.Create(context.Background(), sandock.SandboxSpec{
		Image:     "python:3.12",
		CPUMillis: 500,
		MemoryMiB: 256,
		TimeoutMs: 30_000,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sb.ID != "sb-test001" {
		t.Errorf("expected id sb-test001, got %s", sb.ID)
	}
}

func TestGet(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	client := sandock.NewClient(srv.URL, "test-key")
	sb, err := client.Get(context.Background(), "sb-test001")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if sb.State != "running" {
		t.Errorf("expected state running, got %s", sb.State)
	}
}

func TestList(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	client := sandock.NewClient(srv.URL, "test-key")
	sbs, err := client.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sbs) == 0 {
		t.Error("expected at least one sandbox")
	}
}

func TestExec(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	client := sandock.NewClient(srv.URL, "test-key")
	result, err := client.Exec(context.Background(), "sb-test001", sandock.ExecRequest{
		Command: "echo hello",
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.Stdout != "hello\n" {
		t.Errorf("expected 'hello\\n', got %q", result.Stdout)
	}
	if result.ExitCode != 0 {
		t.Errorf("expected exit code 0, got %d", result.ExitCode)
	}
}

func TestKill(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	client := sandock.NewClient(srv.URL, "test-key")
	if err := client.Kill(context.Background(), "sb-test001"); err != nil {
		t.Fatalf("Kill: %v", err)
	}
}

func TestAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]string{"error": "quota exceeded"})
	}))
	defer srv.Close()

	client := sandock.NewClient(srv.URL, "test-key")
	_, err := client.Create(context.Background(), sandock.SandboxSpec{Image: "python:3.12"})
	if err == nil {
		t.Fatal("expected error")
	}
	apiErr, ok := err.(*sandock.APIError)
	if !ok {
		// Unwrap the wrapped error.
		t.Logf("err type: %T, val: %v", err, err)
	} else if apiErr.StatusCode != http.StatusTooManyRequests {
		t.Errorf("expected 429, got %d", apiErr.StatusCode)
	}
}

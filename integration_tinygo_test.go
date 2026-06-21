package capcompute_test

import (
	"aurora-stores/memory"
	"capcompute"
	"capcompute/dispatcher"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	extism "github.com/extism/go-sdk"
)

type integrationSessionKey struct {
	id string
}

func (k integrationSessionKey) SessionKey() string {
	return k.id
}

type integrationDispatcherFactory struct{}

func (integrationDispatcherFactory) NewDispatcher(context.Context, integrationSessionKey) (dispatcher.Dispatcher[integrationSessionKey], error) {
	return integrationDispatcher{}, nil
}

type integrationDispatcher struct{}

func (integrationDispatcher) Dispatch(_ context.Context, _ integrationSessionKey, call dispatcher.Call) (dispatcher.Outcome, error) {
	switch call.Name {
	case "host.echo":
		return dispatcher.Result(json.RawMessage(`{"echoed":true}`)), nil
	case "host.yield":
		return dispatcher.Yield("waiting for outside work"), nil
	default:
		return dispatcher.Outcome{}, errors.New("unknown call")
	}
}

func TestTinyGoGuestPlayStates(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping TinyGo integration test in short mode")
	}
	if _, err := exec.LookPath("tinygo"); err != nil {
		t.Skip("tinygo not found")
	}

	ctx := context.Background()
	wasmPath := buildTinyGoIntegrationGuest(t)
	store := memory.NewSessionStore[string, integrationSessionKey]()
	compute, err := capcompute.NewComputeCompiledPlugin[string, integrationSessionKey](ctx, capcompute.Config[string, integrationSessionKey]{
		Manifest: extism.Manifest{
			Wasm: []extism.Wasm{extism.WasmFile{Path: wasmPath}},
		},
		PluginConfig: extism.PluginConfig{
			EnableWasi: true,
		},
		Dispatchers:  integrationDispatcherFactory{},
		SessionStore: store,
	})
	if err != nil {
		t.Fatalf("new compute plugin: %v", err)
	}
	t.Cleanup(func() {
		if err := compute.CloseCompiled(context.Background()); err != nil {
			t.Errorf("close compiled plugin: %v", err)
		}
	})

	tests := []struct {
		name string
		want capcompute.PlayStatus
	}{
		{name: "completed", want: capcompute.PlayCompleted},
		{name: "yielded", want: capcompute.PlayYielded},
		{name: "failed", want: capcompute.PlayFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sessionKey := integrationSessionKey{id: "run-" + tt.name}
			input, err := json.Marshal(struct {
				Mode string `json:"mode"`
			}{
				Mode: tt.name,
			})
			if err != nil {
				t.Fatalf("encode input: %v", err)
			}
			session, err := compute.CreateSession(ctx, capcompute.PlayRequest[string, integrationSessionKey]{
				Input:      input,
				Entrypoint: "run",
				UserData:   sessionKey,
			})
			if err != nil {
				t.Fatalf("create session: %v", err)
			}
			t.Cleanup(func() {
				if err := session.Close(context.Background()); err != nil {
					t.Errorf("close session: %v", err)
				}
			})

			if err := store.SaveSession(ctx, sessionKey.SessionKey(), session); err != nil {
				t.Fatalf("save session: %v", err)
			}

			handle, err := compute.Play(ctx, session)
			if err != nil {
				t.Fatalf("play: %v", err)
			}
			result := <-handle.Results()
			if result.Status != tt.want {
				t.Fatalf("status = %s, want %s; err = %v; output = %s", result.Status, tt.want, result.Err, result.Output)
			}
			if tt.want == capcompute.PlayFailed && result.Err == nil {
				t.Fatal("failed play returned nil error")
			}
			if tt.want != capcompute.PlayFailed && result.Err != nil {
				t.Fatalf("play error: %v", result.Err)
			}
		})
	}
}

func TestTinyGoGuestCanBeStopped(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping TinyGo integration test in short mode")
	}
	if _, err := exec.LookPath("tinygo"); err != nil {
		t.Skip("tinygo not found")
	}

	ctx := context.Background()
	wasmPath := buildTinyGoIntegrationGuest(t)
	store := memory.NewSessionStore[string, integrationSessionKey]()
	compute, err := capcompute.NewComputeCompiledPlugin[string, integrationSessionKey](ctx, capcompute.Config[string, integrationSessionKey]{
		Manifest: extism.Manifest{
			Wasm: []extism.Wasm{extism.WasmFile{Path: wasmPath}},
		},
		PluginConfig: extism.PluginConfig{
			EnableWasi: true,
		},
		Dispatchers:  integrationDispatcherFactory{},
		SessionStore: store,
	})
	if err != nil {
		t.Fatalf("new compute plugin: %v", err)
	}
	t.Cleanup(func() {
		if err := compute.CloseCompiled(context.Background()); err != nil {
			t.Errorf("close compiled plugin: %v", err)
		}
	})

	sessionKey := integrationSessionKey{id: "run-stopped"}
	input, err := json.Marshal(struct {
		Mode string `json:"mode"`
	}{
		Mode: "infinite",
	})
	if err != nil {
		t.Fatalf("encode input: %v", err)
	}
	session, err := compute.CreateSession(ctx, capcompute.PlayRequest[string, integrationSessionKey]{
		Input:      input,
		Entrypoint: "run",
		UserData:   sessionKey,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	t.Cleanup(func() {
		if err := session.Close(context.Background()); err != nil {
			t.Errorf("close session: %v", err)
		}
	})
	if err := store.SaveSession(ctx, sessionKey.SessionKey(), session); err != nil {
		t.Fatalf("save session: %v", err)
	}

	handle, err := compute.Play(ctx, session)
	if err != nil {
		t.Fatalf("play: %v", err)
	}
	if _, err := compute.Play(ctx, session); err != capcompute.ErrSessionActive {
		t.Fatalf("concurrent play error = %v, want ErrSessionActive", err)
	}

	handle.Stop()
	handle.Stop()

	results := handle.Results()
	select {
	case result := <-results:
		if result.Status != capcompute.PlayStopped {
			t.Fatalf("status = %s, want %s; err = %v", result.Status, capcompute.PlayStopped, result.Err)
		}
		if !errors.Is(result.Err, context.Canceled) {
			t.Fatalf("error = %v, want context canceled", result.Err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stopped play did not return")
	}
	if _, ok := <-results; ok {
		t.Fatal("stopped play returned more than one result")
	}

	if _, err := compute.Play(ctx, session); err != capcompute.ErrSessionTerminated {
		t.Fatalf("replay error = %v, want ErrSessionTerminated", err)
	}
}

func buildTinyGoIntegrationGuest(t *testing.T) string {
	t.Helper()

	wasmPath := filepath.Join(t.TempDir(), "integration_guest.wasm")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(
		ctx,
		"tinygo",
		"build",
		"-target", "wasip1",
		"-buildmode=c-shared",
		"-tags", "tinygo",
		"-o", wasmPath,
		"./testdata/integration_guest",
	)
	cmd.Env = append(os.Environ(),
		"XDG_CACHE_HOME="+t.TempDir(),
		"GOCACHE="+filepath.Join(t.TempDir(), "go-build"),
	)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("build integration guest timed out: %v", ctx.Err())
	}
	if err != nil {
		t.Fatalf("build integration guest: %v\n%s", err, strings.TrimSpace(string(out)))
	}
	return wasmPath
}

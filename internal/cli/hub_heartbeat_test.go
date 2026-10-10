package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/zzjcool/homer-cli/internal/hub"
	"github.com/zzjcool/homer-cli/internal/stream"
	"github.com/zzjcool/homer-cli/internal/stream/wsconn"
	"github.com/zzjcool/homer-cli/internal/web"
)

func TestReportedAdaptersAgentsRecordsStatusAndPull(t *testing.T) {
	const (
		token   = "heartbeat-test-token"
		agentID = "agent-a"
	)
	registry := hub.NewRegistry()
	sessionOptions := stream.Options{PingInterval: time.Hour, PingTimeout: 2 * time.Hour}
	agentHub := hub.NewAgentHub(registry, &hub.Authenticator{Token: token}, nil, hub.HubOptions{
		HelloTimeout: time.Second,
		Session:      sessionOptions,
	})
	server := httptest.NewServer(agentHub)

	header := make(http.Header)
	header.Set("Authorization", "Bearer "+token)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, _, err := wsconn.Dial(ctx, server.URL+"/agent/v1/stream", wsconn.DialOptions{Header: header})
	if err != nil {
		server.Close()
		t.Fatalf("dial agent stream: %v", err)
	}
	agentSession := stream.NewSession(conn, sessionOptions)
	agentSession.Handle(hub.MethodStatus, func(_ context.Context, _ *stream.Request) (any, error) {
		return json.RawMessage(`{"report":{"adapters":[{"id":"pi"},{"id":"herdr"}]}}`), nil
	})
	agentSession.Handle(hub.MethodPull, func(_ context.Context, _ *stream.Request) (any, error) {
		return json.RawMessage(`{"adapters":[{"id":"opencode"}]}`), nil
	})
	runDone := make(chan error, 1)
	go func() { runDone <- agentSession.Run(ctx) }()
	t.Cleanup(func() {
		agentSession.Close(stream.CloseNormal, "test complete")
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer shutdownCancel()
		_ = agentHub.Shutdown(shutdownCtx)
		server.Close()
		select {
		case <-runDone:
		case <-time.After(time.Second):
			t.Error("agent stream did not stop during cleanup")
		}
	})

	_, err = agentSession.Call(ctx, hub.MethodHello, hub.HelloParams{
		Proto:    stream.ProtocolVersion,
		AgentID:  agentID,
		Hostname: "test-host",
		Caps:     []string{hub.MethodStatus, hub.MethodPull},
	}, stream.WithBudget(time.Second))
	if err != nil {
		t.Fatalf("hello: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		if _, ok := registry.Session(agentID); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("agent session was not attached to the registry")
		}
		time.Sleep(time.Millisecond)
	}

	agents := &reportedAdaptersAgents{Dispatcher: hub.NewDispatcher(registry, "")}
	statusRaw, err := agents.AgentStatus(ctx, agentID)
	if err != nil {
		t.Fatalf("AgentStatus(): %v", err)
	}
	if !json.Valid(statusRaw) {
		t.Fatalf("AgentStatus() returned invalid JSON: %s", statusRaw)
	}
	if got, want := agents.AgentReportedAdapters(agentID), []string{"herdr", "pi"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reported adapters after status = %#v, want %#v", got, want)
	}

	if _, err := agents.AgentPull(ctx, agentID, true, web.SyncScope{}); err != nil {
		t.Fatalf("AgentPull(): %v", err)
	}
	if got, want := agents.AgentReportedAdapters(agentID), []string{"opencode"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reported adapters after pull = %#v, want %#v", got, want)
	}
}

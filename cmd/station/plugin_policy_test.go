package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/libs/plugin-mcp/mcp-host/config"
)

func TestClockPolicyRequiresExplicitRootAndFreshGenerations(t *testing.T) {
	cfg := &config.Config{LogicalServers: []config.LogicalServer{{
		ID: "clock", Transport: config.TransportInprocess,
		Inprocess: &config.InprocessConfig{Command: os.Args[0]},
	}}}
	if _, err := clockInitFactories(context.Background(), cfg, ""); err == nil {
		t.Fatal("implicit root accepted")
	}
	root := t.TempDir()
	factories, err := clockInitFactories(context.Background(), cfg, root)
	if err != nil {
		t.Fatal(err)
	}
	first, err := factories["clock"](context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := factories["clock"](context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Params.Validate(); err != nil {
		t.Fatal(err)
	}
	if first.Params.Grants == nil || len(first.Params.Grants) != 0 || first.Params.HostServices != nil {
		t.Fatal("clock received implicit or reverse authority")
	}
	if first.ExpectedID != "clock-plugin" || first.Params.Incarnation.OwnerID != first.ExpectedID || first.ExpectedVersion != "0.1.0" {
		t.Fatal("policy identity differs from clock artifact")
	}
	if second.Params.Incarnation.HostInstance != first.Params.Incarnation.HostInstance ||
		second.Params.Incarnation.OwnerGeneration <= first.Params.Incarnation.OwnerGeneration {
		t.Fatal("restart reused incarnation")
	}
	if first.Params.DataDir != filepath.Join(root, "clock", "data") || first.Params.CacheDir != filepath.Join(root, "clock", "cache") {
		t.Fatal("owner-selected roots not used")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := factories["clock"](ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled issuance: %v", err)
	}
}

func TestStationDoesNotGuessAnotherPluginPolicy(t *testing.T) {
	cfg := &config.Config{LogicalServers: []config.LogicalServer{{
		ID: "unknown", Transport: config.TransportInprocess,
		Inprocess: &config.InprocessConfig{Command: os.Args[0]},
	}}}
	if _, err := clockInitFactories(context.Background(), cfg, t.TempDir()); err == nil {
		t.Fatal("unknown plugin received clock policy")
	}
	cfg.LogicalServers[0].Transport = config.TransportProcess
	if factories, err := clockInitFactories(context.Background(), cfg, ""); err != nil || len(factories) != 0 {
		t.Fatalf("process-mode configuration changed: %v", err)
	}
}

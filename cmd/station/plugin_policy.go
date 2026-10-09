package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/hollis-labs/libs/plugin-mcp/mcp-host/config"
	pluginhost "github.com/hollis-labs/libs/plugin-mcp/plugin-host"
	"github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/capability"
	"github.com/hollis-labs/libs/plugin-mcp/plugin-sdk/subprocess"
)

// clockInitFactories is Station's explicit policy for its clock demonstration.
// Routing YAML does not issue authority: Station creates a host epoch and uses
// the driver's generation issuer. Other plugins need their own reviewed policy.
// Clock performs no reverse calls and receives an explicit empty grant set.
func clockInitFactories(ctx context.Context, cfg *config.Config, stateDir string) (map[string]config.InprocessInitFactory, error) {
	factories := make(map[string]config.InprocessInitFactory)
	var clock *config.InprocessConfig
	for _, ls := range cfg.LogicalServers {
		if ls.Transport != config.TransportInprocess {
			continue
		}
		if ls.ID != "clock" {
			return nil, fmt.Errorf("inprocess server %q has no Station initialization policy", ls.ID)
		}
		clock = ls.Inprocess
	}
	if clock == nil {
		return factories, nil
	}
	if stateDir == "" {
		return nil, fmt.Errorf("inprocess clock requires -plugin-state-dir")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	command, err := exec.LookPath(clock.Command)
	if err != nil {
		return nil, fmt.Errorf("clock executable: %w", err)
	}
	command, err = filepath.Abs(command)
	if err != nil {
		return nil, err
	}
	stateDir, err = filepath.Abs(stateDir)
	if err != nil {
		return nil, err
	}
	dataDir := filepath.Join(stateDir, "clock", "data")
	cacheDir := filepath.Join(stateDir, "clock", "cache")
	for _, root := range []string{dataDir, cacheDir} {
		if err := os.MkdirAll(root, 0700); err != nil {
			return nil, err
		}
	}
	host, err := pluginhost.NewHostInstance()
	if err != nil {
		return nil, err
	}
	generations := &pluginhost.MemoryGenerationStore{}
	factories["clock"] = func(ctx context.Context) (config.InprocessInitialization, error) {
		generation, err := generations.Next(ctx, host, "clock-plugin")
		if err != nil {
			return config.InprocessInitialization{}, err
		}
		return config.InprocessInitialization{
			ExpectedID: "clock-plugin", ExpectedVersion: "0.1.0",
			Params: subprocess.InitParams{
				PluginDir: filepath.Dir(command), DataDir: dataDir, CacheDir: cacheDir,
				Config: map[string]string{}, LogLevel: "info",
				HostInfo:           subprocess.HostInfo{Version: version, Protocol: subprocess.ProtocolVersion},
				CapabilityContract: capability.ContractVersion,
				Incarnation:        capability.RuntimeIdentity{HostInstance: host, OwnerID: "clock-plugin", OwnerGeneration: generation},
				Grants:             capability.GrantSet{},
			},
		}, nil
	}
	return factories, nil
}

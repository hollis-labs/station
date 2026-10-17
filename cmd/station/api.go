package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/hollis-labs/libs/mcp/plugin-mcp/mcp-host/config"
	pluginhost "github.com/hollis-labs/libs/mcp/plugin-mcp/plugin-host"
	capability "github.com/hollis-labs/libs/mcp/plugin-mcp/plugin-sdk/capability"
	sdksub "github.com/hollis-labs/libs/mcp/plugin-mcp/plugin-sdk/subprocess"

	llmgateway "github.com/hollis-labs/station/plugins/llm-gateway"

	"github.com/hollis-labs/station/platform"
)

// catalog is every plugin this binary can host. A plugin runs only when its
// block exists under `plugins:` in the API config, so listing one here grants
// nothing by itself.
func catalog() []platform.Plugin {
	return []platform.Plugin{llmgateway.New()}
}

// loadAPIApp builds the HTTP API from an api.yaml path. An empty path returns
// nil: Station then only hosts MCP logical servers, as before.
func loadAPIApp(ctx context.Context, path string, logging *platform.Logging) (*platform.App, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := platform.ParseConfig(raw)
	if err != nil {
		return nil, err
	}
	app, err := platform.Assemble(ctx, platform.Options{Config: cfg, Logger: logging.Logger, Sink: logging.Sink(), Plugins: catalog()})
	if err != nil {
		return nil, err
	}
	return app, nil
}

// starterIdentities pins the plugin identity the host expects from each
// inprocess starter. An inprocess logical server with no entry here gets no
// Init factory and fails closed at startup, which is the intended default:
// authority for a new plugin is a code-reviewed decision, not a YAML edit.
var starterIdentities = map[string]struct{ id, version string }{
	"clock": {id: "clock-plugin", version: "0.1.0"},
}

// initFactories builds the trusted per-attempt Init factories mcp-host needs
// for inprocess logical servers. Each plugin gets its own data and cache
// roots under stateDir and an explicit, empty grant set: the starters need no
// host services, so they are delegated no authority.
func initFactories(cfg *config.Config, stateDir string) (map[string]config.InprocessInitFactory, error) {
	hostInstance, err := pluginhost.NewHostInstance()
	if err != nil {
		return nil, err
	}
	out := map[string]config.InprocessInitFactory{}
	for _, ls := range cfg.LogicalServers {
		if ls.Transport != config.TransportInprocess {
			continue
		}
		ident, ok := starterIdentities[ls.ID]
		if !ok {
			continue
		}
		root := filepath.Join(stateDir, "plugins", ls.ID)
		if err := os.MkdirAll(root, 0o700); err != nil {
			return nil, fmt.Errorf("station: create plugin state dir: %w", err)
		}
		var generation atomic.Uint64
		out[ls.ID] = func(context.Context) (config.InprocessInitialization, error) {
			return config.InprocessInitialization{
				Params: sdksub.InitParams{
					PluginDir: root, DataDir: root, CacheDir: root,
					Config: map[string]string{}, LogLevel: "info",
					HostInfo:           sdksub.HostInfo{Version: version, Protocol: sdksub.ProtocolVersion},
					CapabilityContract: capability.ContractVersion,
					Incarnation:        capability.RuntimeIdentity{HostInstance: hostInstance, OwnerID: ident.id, OwnerGeneration: generation.Add(1)},
					Grants:             capability.GrantSet{},
				},
				ExpectedID: ident.id, ExpectedVersion: ident.version,
			}, nil
		}
	}
	return out, nil
}

// defaultStateDir is where plugin data and cache roots live by default.
func defaultStateDir() string {
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "station")
	}
	return filepath.Join(os.TempDir(), "station")
}

// validateLogging is a logger for `station validate`: text, to stderr.
func validateLogging() *platform.Logging {
	l, err := platform.NewLogging(os.Stderr, "text", slog.LevelWarn)
	if err != nil {
		panic(err) // the default redactor configuration cannot fail
	}
	return l
}

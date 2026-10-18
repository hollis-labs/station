package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/hollis-labs/station/platform"
	llmgateway "github.com/hollis-labs/station/plugins/llm-gateway"
	"gopkg.in/yaml.v3"
)

// importGatewayDocument takes only gateway settings and client hashes from
// the bootstrap config. It does not resolve credentials or contact providers.
func importGatewayDocument(path string) (llmgateway.ManagementDocument, error) {
	var d llmgateway.ManagementDocument
	raw, err := os.ReadFile(path)
	if err != nil {
		return d, err
	}
	cfg, err := platform.ParseConfig(raw)
	if err != nil {
		return d, err
	}
	node, ok := cfg.Plugins["llm-gateway"]
	if !ok {
		return d, errors.New("API config has no llm-gateway plugin")
	}
	raw, err = yaml.Marshal(node)
	if err != nil {
		return d, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&d.Gateway); err != nil {
		return d, err
	}
	d.SchemaVersion = llmgateway.ManagementDocumentVersion
	d.Clients = append(d.Clients, cfg.Auth.Keys...)
	if cfg.Auth.KeysFile != "" {
		keys, err := platform.LoadClientKeys(cfg.Auth.KeysFile)
		if err != nil {
			return d, err
		}
		d.Clients = append(d.Clients, keys...)
	}
	return d, d.Validate()
}

// loadGatewayDocument overlays a single active revision. The bootstrap file
// continues to own listener settings, auth mode/JWT and other plugins. A
// configured database must exist and have an active revision; never fall back
// to stale YAML when a database is missing, corrupt or uninitialized.
func loadGatewayDocument(ctx context.Context, cfg *platform.Config, path string) (int64, error) {
	if _, err := os.Stat(path); err != nil {
		return 0, err
	}
	if cfg.Auth.Mode != "" && cfg.Auth.Mode != "enforce" {
		return 0, errors.New("managed gateway requires auth.mode enforce")
	}
	if _, ok := cfg.Plugins["llm-gateway"]; !ok {
		return 0, errors.New("managed gateway requires an llm-gateway plugin block")
	}
	store, err := llmgateway.OpenManagementStore(ctx, path)
	if err != nil {
		return 0, err
	}
	defer store.Close()
	r, err := store.Active(ctx)
	if err != nil {
		return 0, err
	}
	var node yaml.Node
	if err := node.Encode(r.Document.Gateway); err != nil {
		return 0, err
	}
	cfg.Plugins["llm-gateway"] = node
	cfg.Auth.Keys = r.Document.Clients
	cfg.Auth.KeysFile = ""
	return r.ID, nil
}

func runGatewayStore(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: station gateway-store <import|stage|show|status|history|activate> -db /absolute/private/gateway.db [flags]")
		return 2
	}
	action := args[0]
	fs := flag.NewFlagSet("station gateway-store "+action, flag.ContinueOnError)
	db := fs.String("db", "", "absolute path to SQLite database in an owner-only directory")
	api := fs.String("api-config", "", "API YAML to import without resolving secrets")
	input := fs.String("input", "", "versioned JSON document to stage")
	output := fs.String("output", "", "new private JSON output file for show")
	expected := fs.Int64("expected", -1, "required previous latest revision (import/stage), or active revision (activate); 0 means none")
	revision := fs.Int64("revision", 0, "revision to show or activate; show defaults to active")
	actor := fs.String("actor", "", "operator identity recorded in the local audit")
	before := fs.Int64("before", 0, "history cursor: revision IDs below this value")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *db == "" {
		fmt.Fprintln(os.Stderr, "gateway-store requires -db and no positional arguments")
		return 2
	}
	switch action {
	case "import", "stage", "activate":
		if *expected < 0 || *actor == "" {
			fmt.Fprintln(os.Stderr, "mutation requires -expected and -actor")
			return 2
		}
	case "show", "status", "history":
	default:
		fmt.Fprintln(os.Stderr, "unknown gateway-store action")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var doc llmgateway.ManagementDocument
	var err error
	// Validate before opening/creating the database.
	if action == "import" {
		doc, err = importGatewayDocument(*api)
	}
	if action == "stage" {
		var f *os.File
		f, err = os.Open(*input)
		if err == nil {
			var raw []byte
			raw, err = io.ReadAll(io.LimitReader(f, (4<<20)+1))
			f.Close()
			if err == nil {
				doc, err = llmgateway.DecodeManagementDocument(raw)
			}
		}
	}
	if action != "import" && action != "stage" && err == nil {
		_, err = os.Stat(*db)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gateway-store:", err)
		return 1
	}
	s, err := llmgateway.OpenManagementStore(ctx, *db)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gateway-store:", err)
		return 1
	}
	defer s.Close()
	var result any
	switch action {
	case "import", "stage":
		result, err = s.Save(ctx, doc, *expected, *actor)
	case "status":
		result, err = s.Status(ctx)
	case "history":
		result, err = s.History(ctx, *before, 100)
	case "activate":
		if *revision <= 0 {
			err = errors.New("activate requires a positive -revision")
			break
		}
		err = s.Activate(ctx, *revision, *expected, *actor)
		if err == nil {
			result = map[string]any{"active_revision": *revision, "apply": "validate, then reload the service to use this revision"}
		}
	case "show":
		var r llmgateway.ManagementRevision
		if *revision == 0 {
			r, err = s.Active(ctx)
		} else {
			r, err = s.Revision(ctx, *revision)
		}
		if err == nil {
			if *output == "" {
				err = errors.New("show requires -output for a new private document file")
				break
			}
			err = writeGatewayDocument(*output, r.Document)
			result = map[string]any{"revision": r.ID, "output": *output}
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gateway-store:", err)
		return 1
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func writeGatewayDocument(path string, d *llmgateway.ManagementDocument) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	err = enc.Encode(d)
	if err == nil {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
}

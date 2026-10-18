package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hollis-labs/station/platform"
	llmgateway "github.com/hollis-labs/station/plugins/llm-gateway"
	"gopkg.in/yaml.v3"
)

// runClient manages hashed gateway credentials offline. Reload the service to
// activate changes. A token is written only to a new private file, never stdout.
func runClient(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: station client issue|revoke|list (-keys-file PATH | -gateway-db PATH) [-id APP -output TOKEN_FILE -scopes CSV -expected REV -actor OPERATOR]")
		return 2
	}
	verb := args[0]
	fs := flag.NewFlagSet("station client "+verb, flag.ContinueOnError)
	database := fs.String("gateway-db", "", "SQLite gateway database; mutually exclusive with keys-file")
	expected := fs.Int64("expected", -1, "current latest revision for a SQLite credential change")
	actor := fs.String("actor", "", "operator identity for SQLite revision audit")
	registry := fs.String("keys-file", "", "absolute path to hashed client registry")
	id := fs.String("id", "", "stable application principal")
	output := fs.String("output", "", "new private file receiving the token")
	scopes := fs.String("scopes", "llm-gateway.chat,llm-gateway.models.read", "comma-separated scopes")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() != 0 || (*database == "" && !filepath.IsAbs(*registry)) || (*database != "" && (*registry != "" || !filepath.IsAbs(*database))) {
		fmt.Fprintln(os.Stderr, "station: provide exactly one absolute -keys-file or -gateway-db")
		return 2
	}
	if verb != "issue" && verb != "revoke" && verb != "list" {
		fmt.Fprintln(os.Stderr, "station: unknown client operation")
		return 2
	}
	var err error
	if *database != "" {
		if verb != "list" && (*expected < 0 || *actor == "") {
			fmt.Fprintln(os.Stderr, "station: SQLite client changes require -expected and -actor")
			return 2
		}
		err = manageStoredClient(verb, *database, *id, *output, *scopes, *expected, *actor)
	} else {
		err = manageClient(verb, *registry, *id, *output, *scopes)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "station: client:", err)
		return 1
	}
	return 0
}

func manageClient(verb, registry, id, output, scopes string) error {
	lock, err := os.OpenFile(registry+".lock", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("lock registry (if stale, inspect before removing %s.lock): %w", registry, err)
	}
	_ = lock.Close()
	defer os.Remove(registry + ".lock")
	keys, err := platform.LoadClientKeys(registry)
	if errors.Is(err, os.ErrNotExist) && verb == "issue" {
		err = nil
	}
	if err != nil {
		return err
	}
	return editClientKeys(verb, keys, id, output, scopes, func(keys []platform.KeyConfig) error { return saveClientKeys(registry, keys) }, "reload the gateway to activate")
}

func editClientKeys(verb string, keys []platform.KeyConfig, id, output, scopes string, save func([]platform.KeyConfig) error, nextStep string) error {
	if verb == "list" {
		for _, k := range keys {
			fmt.Printf("%s\t%s\n", k.ID, strings.Join(k.Scopes, ","))
		}
		return nil
	}
	pos := -1
	for i, k := range keys {
		if k.ID == id {
			pos = i
		}
	}
	switch verb {
	case "issue":
		if pos >= 0 {
			return fmt.Errorf("client already exists; revoke it before reissuing")
		}
		if output == "" || !filepath.IsAbs(output) {
			return fmt.Errorf("-output must be a new absolute file path")
		}
		var b [32]byte
		if _, err := rand.Read(b[:]); err != nil {
			return err
		}
		token := "stn_" + hex.EncodeToString(b[:])
		sum := sha256.Sum256([]byte(token))
		list := strings.Split(scopes, ",")
		for i := range list {
			list[i] = strings.TrimSpace(list[i])
		}
		keys = append(keys, platform.KeyConfig{ID: id, SHA256: hex.EncodeToString(sum[:]), Scopes: list})
		if err := platform.ValidateClientKeys(keys); err != nil {
			return err
		}
		f, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, err = f.WriteString(token + "\n")
		if err == nil {
			err = f.Sync()
		}
		err = errors.Join(err, f.Close())
		if err != nil {
			_ = os.Remove(output)
			return err
		}
		if err := save(keys); err != nil {
			_ = os.Remove(output)
			return err
		}
		fmt.Printf("station: issued client %s; token saved to %s (0600); %s\n", id, output, nextStep)
		return nil
	case "revoke":
		if pos < 0 {
			return fmt.Errorf("client does not exist")
		}
		keys = append(keys[:pos], keys[pos+1:]...)
	}
	if err := save(keys); err != nil {
		return err
	}
	fmt.Printf("station: revoked client %s; %s\n", id, nextStep)
	return nil
}

func saveClientKeys(path string, keys []platform.KeyConfig) error {
	raw, err := yaml.Marshal(keys)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".client-keys-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(raw)
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// SQLite credential changes stage a revision, just like configuration edits.
// A separate activation prevents accidentally applying other pending edits.
func manageStoredClient(verb, database, id, output, scopes string, expected int64, actor string) error {
	if _, err := os.Stat(database); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := llmgateway.OpenManagementStore(ctx, database)
	if err != nil {
		return err
	}
	defer s.Close()
	status, err := s.Status(ctx)
	if err != nil {
		return err
	}
	if verb != "list" && expected != status.LatestRevision {
		return llmgateway.ErrRevisionConflict
	}
	r, err := s.Revision(ctx, status.LatestRevision)
	if err != nil {
		return err
	}
	if verb == "list" {
		fmt.Printf("station: latest revision %d; active revision %d\n", status.LatestRevision, status.ActiveRevision)
	}
	return editClientKeys(verb, r.Document.Clients, id, output, scopes, func(keys []platform.KeyConfig) error {
		r.Document.Clients = keys
		saved, err := s.Save(ctx, *r.Document, expected, actor)
		if err == nil {
			fmt.Printf("station: staged gateway revision %d\n", saved.ID)
		}
		return err
	}, "activate the staged revision, validate, then reload the gateway")
}

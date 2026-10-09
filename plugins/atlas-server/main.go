// Command atlas-server is a process-mode MCP server for the Portfolio Atlas:
// tools scoped to the Atlas namespace in Tesseract, so an agent can read the
// register without knowing the namespace, the key scheme or the recall API.
// This is the read slice: nothing here writes to Tesseract.
//
// It is a real, standalone MCP server over stdio with no dependency on Station
// or mcp-host; Station typically spawns and supervises it as a process-mode
// logical server (see ../../station.yaml). Because Station forwards a logical
// server's tools but sets MCP instructions from the config `description`, the
// operating guide is also a tool (atlas_guide), not only instructions.
//
// Configuration is environment only:
//
//	TESSERACT_URL    Tesseract's API (default http://127.0.0.1:8089)
//	TESSERACT_TOKEN  bearer token, if that Tesseract requires one (default none)
//	ATLAS_CACHE_TTL  how long the loaded register is reused, a Go duration
//	                 (default 30s; 0 reads Tesseract on every call)
package main

import (
	"context"
	"log"
	"os"
	"time"

	gmcpserver "github.com/hollis-labs/libs/plugin-mcp/go-mcp/server"

	"github.com/hollis-labs/station/internal/atlas/corpus"
	"github.com/hollis-labs/station/internal/atlas/tesseract"
	"github.com/hollis-labs/station/internal/atlas/tools"
)

const version = "0.1.0"

const instructions = "Portfolio Atlas tools, read-only. Call atlas_guide first: it explains the record classes, the key scheme " +
	"(ATLAS-Q-014 is a Question), the vocabularies and what does not belong in the Atlas. Every tool is scoped to the Atlas namespace; " +
	"none takes a namespace. Records are shown as stored, including where they disagree with the contract: report that, do not fix it in passing."

func main() {
	ttl := 30 * time.Second
	if v := os.Getenv("ATLAS_CACHE_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			log.Fatalf("atlas-server: ATLAS_CACHE_TTL %q: %v", v, err)
		}
		ttl = d
	}
	client := tesseract.New(os.Getenv("TESSERACT_URL"), os.Getenv("TESSERACT_TOKEN"))

	srv := gmcpserver.NewServer("atlas-server", version, gmcpserver.WithInstructions(instructions))
	tools.Register(srv, tools.Deps{Client: client, Store: corpus.NewStore(client, ttl)})

	if err := srv.Run(context.Background()); err != nil {
		log.Fatalf("atlas-server: %v", err)
	}
}

package main

import (
	"github.com/hollis-labs/station/internal/appboot"
)

// afterConfig and plan are the shared boot hook and command plan (internal/appboot) with this
// binary's module order. `station` serves with no command, like `station serve`.
var afterConfig = appboot.AfterConfig

func plan(args []string) (modules []string, prepare bool) {
	return appboot.Plan(args, substrateModules, []string{"serve"}, nil)
}

var (
	readOnly     = appboot.ReadOnly
	withSecrets  = appboot.WithSecrets
	withIdentity = appboot.WithIdentity
)

func pick(want []string) []string { return appboot.Pick(substrateModules, want) }

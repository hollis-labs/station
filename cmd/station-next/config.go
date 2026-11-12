package main

import (
	"github.com/hollis-labs/station/internal/appboot"
)

// plan is the shared command plan (internal/appboot) with this binary's module order. Only
// `station serve` serves; `station` alone prints help.
func plan(args []string) (modules []string, mode appboot.Mode) {
	return appboot.Plan(args, substrateModules, nil)
}

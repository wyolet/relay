package main

import (
	"fmt"
	"log"

	"github.com/wyolet/relay/pkg/crypto"
)

// runMasterKey handles the "relay master-key <subcommand>" family.
// Currently only "generate" is supported.
//
// Usage:
//
//	relay master-key generate   — print a fresh base64 RELAY_MASTER_KEY to stdout
func runMasterKey(args []string) {
	if len(args) == 0 {
		log.Fatal("usage: relay master-key <generate>")
	}
	switch args[0] {
	case "generate":
		key, err := crypto.GenerateMasterKey()
		if err != nil {
			log.Fatalf("master-key generate: %v", err)
		}
		fmt.Println(key)
	default:
		log.Fatalf("master-key: unknown subcommand %q", args[0])
	}
}

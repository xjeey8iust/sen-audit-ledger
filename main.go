// Command sen-audit-ledger serves the HTTP API described in README.md.
package main

import (
	"log"
	"os"

	"github.com/xjeey8iust/sen-audit-ledger/internal/api"
	"github.com/xjeey8iust/sen-audit-ledger/internal/store"
)

func main() {
	address := os.Getenv("ADDR")
	if address == "" {
		address = "127.0.0.1:8080"
	}
	databasePath := os.Getenv("DB_PATH")
	if databasePath == "" {
		databasePath = "sen-audit-ledger.db"
	}

	st, err := store.Open(databasePath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	if err := api.NewRouter(st).Run(address); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

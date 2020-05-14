package main

import (
	"encoding/json"
	"os"

	"github.com/open-policy-agent/opa/ast"
)

func main() {

	f := ast.NewFeaturesForThisVersion()

	fd, err := os.Create(os.Args[1])
	if err != nil {
		panic(err)
	}

	enc := json.NewEncoder(fd)
	enc.SetIndent("", "  ")

	if err := enc.Encode(f); err != nil {
		panic(err)
	}

	if err := fd.Close(); err != nil {
		panic(err)
	}
}

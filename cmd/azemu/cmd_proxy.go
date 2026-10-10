package main

import (
	"flag"

	"github.com/luigi23997/azemu/internal/contract"
	"github.com/luigi23997/azemu/internal/proxy"
)

func runProxy(args []string) error {
	fs := flag.NewFlagSet("proxy", flag.ExitOnError)
	cfg := fs.String("config", contract.ProxyConfigPath, "path to proxy.json")
	fs.Parse(args)
	return proxy.Run(*cfg)
}

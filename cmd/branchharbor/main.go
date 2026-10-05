// BranchHarbor is an experimental, Linux-only local workspace store.
package main

import (
	"branchharbor/internal/api"
	"branchharbor/internal/auth"
	"branchharbor/internal/store"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "branchharbor:", err)
		os.Exit(1)
	}
}
func run(args []string) error {
	if runtime.GOOS != "linux" {
		return errors.New("Linux is required; use WSL2 on Windows")
	}
	if len(args) == 0 {
		return errors.New("usage: branchharbor init|serve|token|gc|inspect [flags]")
	}
	cmd := args[0]
	f := flag.NewFlagSet(cmd, flag.ContinueOnError)
	dir := f.String("dir", ".bh", "private store directory")
	listen := f.String("listen", "127.0.0.1:8787", "loopback listener")
	sub := f.String("subject", "operator", "token subject")
	branch := f.String("branch", "*", "token branch")
	prefix := f.String("prefix", "", "directory prefix")
	ops := f.String("ops", "admin", "comma-separated permissions")
	ttl := f.Int64("ttl", 900, "token lifetime seconds, maximum 3600")
	out := f.String("out", "", "new private token output file")
	apply := f.Bool("apply", false, "apply orphan collection")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	switch cmd {
	case "init", "serve", "token", "gc", "inspect":
	default:
		return errors.New("unknown command")
	}
	if cmd == "token" {
		if *out == "" || *ttl < 1 || *ttl > 3600 {
			return errors.New("token requires --out and TTL 1..3600")
		}
		key, err := auth.ReadKey(*dir)
		if err != nil {
			return err
		}
		now := time.Now()
		c := auth.Claims{Schema: 1, Sub: *sub, Branch: *branch, Prefix: *prefix, Ops: strings.Split(*ops, ","), Iat: now.Unix(), Exp: now.Unix() + *ttl}
		token, err := auth.Mint(key, c, now)
		if err != nil {
			return err
		}
		if err = auth.WritePrivate(*out, []byte(token+"\n")); err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]bool{"token_written": true})
	}
	if cmd != "init" {
		if _, err := os.Lstat(filepath.Join(*dir, "FORMAT")); err != nil {
			return errors.New("store is not initialized; run init explicitly")
		}
	}
	s, err := store.Open(*dir)
	if err != nil {
		return err
	}
	defer s.Close()
	switch cmd {
	case "init":
		if err = auth.InitKey(*dir); err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]bool{"initialized": true})
	case "inspect":
		b, err := s.Branches()
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(b)
	case "gc":
		r, err := s.GC(*apply)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(r)
	case "serve":
		key, err := auth.ReadKey(*dir)
		if err != nil {
			return err
		}
		ln, err := api.Listen(*listen)
		if err != nil {
			return err
		}
		defer ln.Close()
		server := api.Server(api.New(s, key))
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		done := make(chan error, 1)
		go func() { done <- server.Serve(ln) }()
		if err = json.NewEncoder(os.Stdout).Encode(map[string]string{"listening": ln.Addr().String()}); err != nil {
			server.Close()
			return err
		}
		select {
		case err = <-done:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case <-ctx.Done():
			timeout, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err = server.Shutdown(timeout); err != nil {
				server.Close()
				return err
			}
			err = <-done
			if !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		}
	}
	return nil
}

// board-preview serves the real board on isolated synthetic data, without a pipeline.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zachlatta/agent-factory/factory"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:0", "listen address; use this host's tailnet IP for remote computer use")
	scratch := flag.String("scratch", ".factory-scratch", "worktree scratch directory")
	flag.Parse()
	if err := run(*listen, *scratch); err != nil {
		log.Fatal(err)
	}
}

func run(address, scratch string) error {
	if err := os.MkdirAll(scratch, 0700); err != nil {
		return err
	}
	root, err := os.MkdirTemp(scratch, "board-preview-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	h, err := factory.PreviewHandler(root)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	defer ln.Close()
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	// One machine-readable line; the screenshot helper owns this process and address.
	fmt.Printf("http://%s\n", ln.Addr())
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil {
			return err
		}
		<-done
	}
	return nil
}

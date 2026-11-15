// Command bankcmp compares a legacy bank database against its migrated
// replacement, table by table, and proves the difference list is exact.
package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"bankcmp/internal/api"
	"bankcmp/internal/app"
	"bankcmp/internal/journal"
)

//go:embed static
var staticFS embed.FS

var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "compare" {
		os.Exit(runCompare(os.Args[2:]))
	}
	var (
		addr        = flag.String("addr", "127.0.0.1:8080", "HTTP listen address")
		journalPath = flag.String("journal", "bankcmp.db", "journal database path")
		parallel    = flag.Int("parallel", 1, "tasks compared at once")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	if err := run(*addr, *journalPath, *parallel); err != nil {
		log.Fatal(err)
	}
}

func run(addr, journalPath string, parallel int) error {
	store, err := journal.Open(journalPath)
	if err != nil {
		return fmt.Errorf("open journal: %w", err)
	}
	defer store.Close()

	a := app.New(store, parallel)
	a.Version = version
	srv := &api.Server{App: a, Store: store}

	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		return err
	}
	handler := srv.Handler(http.FileServer(http.FS(static)))

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	// Print the actual port, which matters when addr asked for :0.
	fmt.Printf("bankcmp %s listening on http://%s (journal %s)\n", version, ln.Addr(), abs(journalPath))
	go func() {
		if err := httpServer.Serve(ln); err != http.ErrServerClosed {
			log.Printf("serve: %v", err)
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return httpServer.Shutdown(ctx)
}

func abs(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

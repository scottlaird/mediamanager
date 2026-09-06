// Command mm imports media from cameras and cards, stages it through local
// spools onto a NAS, and keeps the link tree the editor sees pointing at
// the best copy.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/scottlaird/mediamanager/catalog"
	"github.com/scottlaird/mediamanager/config"
	"github.com/scottlaird/mediamanager/ingest"
	"github.com/scottlaird/mediamanager/volume"
	"github.com/spf13/cobra"
)

var (
	configPath string
	verbose    bool
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigs
		// First signal: cancel and let the current buffer finish so the
		// .partial and the catalog agree. A copy blocked inside a slow NAS
		// syscall cannot notice until it returns, so a second signal quits
		// outright; the partial is picked up next run either way.
		fmt.Fprintln(os.Stderr, "mm: stopping after the current buffer; press Ctrl-C again to quit now (the copy resumes next run)")
		cancel()
		<-sigs
		os.Exit(130)
	}()
	if err := root().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "mm:", err)
		os.Exit(1)
	}
}

func root() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "mm",
		Short:         "Import and stage camera media across spools and a NAS",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.PersistentFlags().StringVar(&configPath, "config", "", "config file (default "+config.DefaultPath()+")")
	cmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "log every copy and progress")
	cmd.AddCommand(importCmd(), adoptCmd(), archiveCmd(), flushCmd(), relinkCmd(), statusCmd(), lsCmd(), pinCmd(true), pinCmd(false), volumeCmd(), workerCmd())
	return cmd
}

// openEnv loads config, opens the catalog and wires the platform volume
// helpers. The returned func closes the catalog.
func openEnv() (*ingest.Env, func(), error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(dirOf(cfg.Catalog), 0o755); err != nil {
		return nil, nil, err
	}
	cat, err := catalog.Open(cfg.Catalog)
	if err != nil {
		return nil, nil, err
	}
	env := &ingest.Env{
		Config:   cfg,
		Catalog:  cat,
		Find:     volume.Find,
		Identify: volume.Identify,
	}
	if verbose {
		env.Logf = func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) }
	}
	return env, func() { cat.Close() }, nil
}

func dirOf(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[:i]
		}
	}
	return "."
}

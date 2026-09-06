package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/scottlaird/mediamanager/catalog"
	"github.com/scottlaird/mediamanager/ingest"
	"github.com/scottlaird/mediamanager/volume"
	"github.com/spf13/cobra"
)

func importCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "import <source>...",
		Short: "Register, link, spool and archive everything on one or more sources",
		Long: `Import scans each source (a mounted card or camera), gives every new file
its permanent name, links it into the link tree immediately, then copies it
to a spool and on to the NAS. Sources are processed concurrently, one copy
at a time per source. Re-running on the same source is safe and cheap.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, done, err := openEnv()
			if err != nil {
				return err
			}
			defer done()
			var (
				wg   sync.WaitGroup
				mu   sync.Mutex
				fail bool
			)
			for _, src := range args {
				wg.Add(1)
				go func(src string) {
					defer wg.Done()
					sum, err := env.Import(cmd.Context(), src)
					mu.Lock()
					defer mu.Unlock()
					if err != nil {
						fmt.Fprintf(os.Stderr, "%s: %v\n", src, err)
						fail = true
						return
					}
					printSummary(os.Stdout, src, sum)
					if len(sum.Failures) > 0 {
						fail = true
					}
				}(src)
			}
			wg.Wait()
			if fail {
				return errors.New("import finished with errors")
			}
			return nil
		},
	}
}

func printSummary(w io.Writer, src string, s *ingest.Summary) {
	fmt.Fprintf(w, "%s (%s): %d assets, %d new, %d spooled, %d archived\n",
		src, s.Source.Shape, len(s.Source.Assets), len(s.Source.New), s.Spooled, s.Archived)
	for _, rel := range s.Source.Unrouted {
		fmt.Fprintf(w, "  skipped (no tree for its kind): %s\n", rel)
	}
	for _, rel := range s.Source.OrphanProxies {
		fmt.Fprintf(w, "  proxy without an original: %s\n", rel)
	}
	if len(s.Source.Unrecognised) > 0 {
		fmt.Fprintf(w, "  %d unrecognised files ignored\n", len(s.Source.Unrecognised))
	}
	for _, id := range s.SpoolFull {
		fmt.Fprintf(w, "  spool full, archived from source: %s\n", id)
	}
	for _, f := range s.Failures {
		fmt.Fprintf(w, "  FAILED %s %s: %v\n", f.Step, f.AssetID, f.Err)
	}
	if s.SafeToFormat {
		fmt.Fprintf(w, "  every file is on the NAS; safe to format in camera\n")
	} else {
		fmt.Fprintf(w, "  NOT safe to format: some files are not on the NAS yet\n")
	}
}

func archiveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "archive",
		Short: "Copy every asset that is not yet on the NAS, from wherever it is",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			env, done, err := openEnv()
			if err != nil {
				return err
			}
			defer done()
			sum, err := env.ArchiveAll(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Printf("%d archived\n", sum.Archived)
			for _, f := range sum.Failures {
				fmt.Printf("  FAILED %s: %v\n", f.AssetID, f.Err)
			}
			if len(sum.Failures) > 0 {
				return errors.New("archive finished with errors")
			}
			return nil
		},
	}
}

func flushCmd() *cobra.Command {
	var (
		free   string
		older  string
		dryRun bool
	)
	cmd := &cobra.Command{
		Use:   "flush <spool>",
		Short: "Delete spool copies that are verified to be on the NAS, oldest first",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var opts ingest.FlushOptions
			var err error
			if free != "" {
				if opts.FreeBytes, err = parseSize(free); err != nil {
					return err
				}
			}
			if older != "" {
				d, err := parseAge(older)
				if err != nil {
					return err
				}
				opts.OlderThan = time.Now().Add(-d)
			}
			opts.DryRun = dryRun
			env, done, err := openEnv()
			if err != nil {
				return err
			}
			defer done()
			rep, err := env.FlushSpool(cmd.Context(), args[0], opts)
			if err != nil {
				return err
			}
			verb := "flushed"
			if dryRun {
				verb = "would flush"
			}
			fmt.Printf("%s %d assets, %s\n", verb, len(rep.Flushed), humanSize(rep.Freed))
			for id, reason := range rep.Refused {
				fmt.Printf("  kept %s: %s\n", id, reason)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&free, "free", "", "stop once this much is freed, e.g. 2T or 500G (default: everything eligible)")
	cmd.Flags().StringVar(&older, "older-than", "", "only assets captured longer ago than this, e.g. 30d or 12h")
	cmd.Flags().BoolVarP(&dryRun, "dry-run", "n", false, "report without deleting")
	return cmd
}

func relinkCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "relink",
		Short: "Audit the link trees and repoint every link at its best copy",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			env, done, err := openEnv()
			if err != nil {
				return err
			}
			defer done()
			rep, err := env.Relink(cmd.Context())
			if err != nil {
				return err
			}
			for kind, r := range rep.Trees {
				fmt.Printf("%s: %d created, %d updated, %d unchanged, %d unknown links\n",
					kind, len(r.Created), len(r.Updated), r.Unchanged, len(r.Unknown))
				for _, u := range r.Unknown {
					fmt.Printf("  unknown link: %s\n", u)
				}
			}
			if len(rep.Unavailable) > 0 {
				fmt.Printf("%d assets have no mounted copy right now\n", len(rep.Unavailable))
			}
			return nil
		},
	}
}

func statusCmd() *cobra.Command {
	var assets bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show locations, and with --assets every asset and where it is",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			env, done, err := openEnv()
			if err != nil {
				return err
			}
			defer done()
			st, err := env.Status(cmd.Context())
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "LOCATION\tKIND\tMOUNTED\tFREE\tROOT")
			for _, l := range st.Locations {
				mounted, free := "no", "-"
				if l.Mounted {
					mounted, free = "yes", humanSize(l.Free)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", l.Name, l.Kind, mounted, free, l.Root)
			}
			tw.Flush()
			counts := map[string]int{}
			for _, a := range st.Assets {
				counts[a.State]++
			}
			fmt.Printf("\n%d assets:", len(st.Assets))
			for _, s := range []string{"discovered", "spooled", "archived", "flushed", "unavailable"} {
				if counts[s] > 0 {
					fmt.Printf(" %d %s", counts[s], s)
				}
			}
			fmt.Println()
			if !assets {
				return nil
			}
			tw = tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "\nSTATE\tSIZE\tPATH\tCOPIES")
			for _, a := range st.Assets {
				pin := ""
				if a.Asset.Pinned {
					pin = " (pinned)"
				}
				fmt.Fprintf(tw, "%s%s\t%s\t%s/%s\t%s\n", a.State, pin, humanSize(a.Asset.Size), a.Asset.Kind, a.Asset.RelPath, strings.Join(a.Copies, ","))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&assets, "assets", false, "list every asset")
	return cmd
}

func pinCmd(pin bool) *cobra.Command {
	name, short := "pin", "Keep an asset in the spool through flushes"
	if !pin {
		name, short = "unpin", "Let an asset be flushed again"
	}
	return &cobra.Command{
		Use:   name + " <asset-id>...",
		Short: short,
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, done, err := openEnv()
			if err != nil {
				return err
			}
			defer done()
			for _, id := range args {
				if err := env.Catalog.SetPinned(cmd.Context(), id, pin); err != nil {
					if errors.Is(err, catalog.ErrNotFound) {
						return fmt.Errorf("no asset %s", id)
					}
					return err
				}
			}
			return nil
		},
	}
}

func volumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "volume <path>",
		Short: "Show the volume UUID and label behind a path, for the config file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			info, err := volume.Identify(args[0])
			if err != nil {
				return err
			}
			fmt.Printf("mount:  %s\nlabel:  %s\nuuid:   %s\n", info.MountPoint, info.Label, info.UUID)
			return nil
		},
	}
}

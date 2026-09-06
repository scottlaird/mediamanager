package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/scottlaird/mediamanager/ingest"
	"github.com/scottlaird/mediamanager/media"
	"github.com/spf13/cobra"
)

func lsCmd() *cobra.Command {
	var (
		states   []string
		location string
		kind     string
		long     bool
		asJSON   bool
	)
	cmd := &cobra.Command{
		Use:   "ls [prefix...]",
		Short: "List assets, optionally under a path prefix",
		Long: `List assets in directory order. A prefix matches the start of an asset's
path (2026/09) or of kind/path (still/2026). Filters combine.

  mm ls 2026/07                      everything shot in July 2026
  mm ls --state spooled              not yet on the NAS
  mm ls --location m2 --long         what is on m2, with copies and sidecars
  mm ls --json | jq ...`,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts := ingest.ListOptions{Prefixes: args, States: states, Location: location, Companions: long || asJSON}
			switch kind {
			case "":
			case "video":
				opts.Kind = media.Video
			case "audio":
				opts.Kind = media.Audio
			case "still":
				opts.Kind = media.Still
			default:
				return fmt.Errorf("--kind must be video, audio or still, not %q", kind)
			}
			env, done, err := openEnv()
			if err != nil {
				return err
			}
			defer done()
			assets, err := env.List(cmd.Context(), opts)
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSON(assets)
			}
			printAssets(assets, long)
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&states, "state", nil, "keep these states: discovered, spooled, archived, flushed, unavailable")
	cmd.Flags().StringVar(&location, "location", "", "keep assets with a complete copy on this location")
	cmd.Flags().StringVar(&kind, "kind", "", "video, audio or still")
	cmd.Flags().BoolVarP(&long, "long", "l", false, "show id, capture time, every copy and companion")
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	return cmd
}

func printAssets(assets []ingest.AssetStatus, long bool) {
	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	defer tw.Flush()
	if !long {
		fmt.Fprintln(tw, "STATE\tSIZE\tPATH\tCOPIES")
		for _, a := range assets {
			fmt.Fprintf(tw, "%s%s\t%s\t%s/%s\t%s\n", a.State, pinMark(a), humanSize(a.Asset.Size), a.Asset.Kind, a.Asset.RelPath, strings.Join(a.Copies, ","))
		}
		return
	}
	var total int64
	for i, a := range assets {
		if i > 0 {
			fmt.Fprintln(tw)
		}
		total += a.Asset.Size
		fmt.Fprintf(tw, "%s/%s\n", a.Asset.Kind, a.Asset.RelPath)
		fmt.Fprintf(tw, "  state\t%s%s\n  id\t%s (%s)\n  size\t%s\n  captured\t%s\n  original\t%s\n",
			a.State, pinMark(a), a.Asset.ID, a.Asset.Scheme, humanSize(a.Asset.Size),
			a.Asset.CaptureTime.Format("2006-01-02 15:04:05 MST"), a.Asset.OrigName)
		if a.Asset.Project != "" {
			fmt.Fprintf(tw, "  project\t%s\n", a.Asset.Project)
		}
		for _, cp := range a.CopyRows {
			verified := ""
			if !cp.VerifiedAt.IsZero() {
				verified = " verified " + cp.VerifiedAt.Format(time.DateOnly)
			}
			fmt.Fprintf(tw, "  copy\t%s\t%s\t%s%s\n", cp.Location.Name, cp.State, cp.RelPath, verified)
		}
		for _, c := range a.Companions {
			fmt.Fprintf(tw, "  %s\t%s\t\t%s\n", c.Role, locationName(a, c.LocationID), c.RelPath)
		}
	}
	if len(assets) > 1 {
		fmt.Fprintf(tw, "\n%d assets, %s\n", len(assets), humanSize(total))
	}
}

func pinMark(a ingest.AssetStatus) string {
	if a.Asset.Pinned {
		return " (pinned)"
	}
	return ""
}

func locationName(a ingest.AssetStatus, id int64) string {
	for _, cp := range a.CopyRows {
		if cp.LocationID == id {
			return cp.Location.Name
		}
	}
	return fmt.Sprintf("location %d", id)
}

// jsonAsset is the stable machine-readable shape; it does not expose the
// catalog structs directly so those can change freely.
type jsonAsset struct {
	ID          string          `json:"id"`
	Scheme      string          `json:"scheme"`
	Kind        string          `json:"kind"`
	Path        string          `json:"path"`
	State       string          `json:"state"`
	Size        int64           `json:"size"`
	CaptureTime time.Time       `json:"capture_time"`
	Original    string          `json:"original_name"`
	Project     string          `json:"project,omitempty"`
	Pinned      bool            `json:"pinned"`
	Copies      []jsonCopy      `json:"copies"`
	Companions  []jsonCompanion `json:"companions"`
}

type jsonCopy struct {
	Location   string     `json:"location"`
	Kind       string     `json:"location_kind"`
	Path       string     `json:"path"`
	State      string     `json:"state"`
	VerifiedAt *time.Time `json:"verified_at,omitempty"`
}

type jsonCompanion struct {
	Role     string `json:"role"`
	Ext      string `json:"ext"`
	Location string `json:"location"`
	Path     string `json:"path"`
}

func writeJSON(assets []ingest.AssetStatus) error {
	out := make([]jsonAsset, 0, len(assets))
	for _, a := range assets {
		ja := jsonAsset{
			ID: a.Asset.ID, Scheme: a.Asset.Scheme, Kind: a.Asset.Kind.String(), Path: a.Asset.RelPath,
			State: a.State, Size: a.Asset.Size, CaptureTime: a.Asset.CaptureTime, Original: a.Asset.OrigName,
			Project: a.Asset.Project, Pinned: a.Asset.Pinned,
			Copies: []jsonCopy{}, Companions: []jsonCompanion{},
		}
		for _, cp := range a.CopyRows {
			jc := jsonCopy{Location: cp.Location.Name, Kind: string(cp.Location.Kind), Path: cp.RelPath, State: string(cp.State)}
			if !cp.VerifiedAt.IsZero() {
				t := cp.VerifiedAt
				jc.VerifiedAt = &t
			}
			ja.Copies = append(ja.Copies, jc)
		}
		for _, c := range a.Companions {
			ja.Companions = append(ja.Companions, jsonCompanion{Role: string(c.Role), Ext: c.Ext, Location: locationName(a, c.LocationID), Path: c.RelPath})
		}
		out = append(out, ja)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

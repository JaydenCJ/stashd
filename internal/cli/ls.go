// The query path: `stashd ls` and `stashd info`.
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/JaydenCJ/stashd/internal/index"
	"github.com/JaydenCJ/stashd/internal/spec"
	"github.com/JaydenCJ/stashd/internal/store"
)

func cmdLs(args []string, stdout, stderr io.Writer) int {
	fs, storeDir := newFlagSet("ls", stderr)
	run := fs.String("run", "", "only artifacts from runs matching this glob")
	nameGlob := fs.String("name", "", "only artifacts whose name matches this glob")
	pinned := fs.Bool("pinned", false, "only pinned artifacts")
	asJSON := fs.Bool("json", false, "emit a JSON array instead of a table")
	var tags repeatedFlag
	fs.Var(&tags, "tag", "only artifacts carrying this key=value tag (repeatable)")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if fs.NArg() != 0 {
		return usageErr(stderr, "ls takes no positional arguments")
	}
	want := map[string]string{}
	for _, t := range tags {
		k, v, err := spec.ParseTag(t)
		if err != nil {
			return usageErr(stderr, "%v", err)
		}
		want[k] = v
	}

	st, err := openStore(*storeDir)
	if err != nil {
		return runtimeErr(stderr, err)
	}
	arts, err := st.Index.List()
	if err != nil {
		return runtimeErr(stderr, err)
	}
	var kept []*index.Artifact
	for _, a := range arts {
		if *run != "" && !spec.GlobMatch(*run, a.Run) {
			continue
		}
		if *nameGlob != "" && !spec.GlobMatch(*nameGlob, a.Name) {
			continue
		}
		if *pinned && !a.Pinned {
			continue
		}
		match := true
		for k, v := range want {
			if a.Tags[k] != v {
				match = false
				break
			}
		}
		if match {
			kept = append(kept, a)
		}
	}
	store.SortNewestFirst(kept)

	if *asJSON {
		if kept == nil {
			kept = []*index.Artifact{}
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		enc.Encode(kept)
		return exitOK
	}
	if len(kept) == 0 {
		fmt.Fprintln(stdout, "no artifacts")
		return exitOK
	}
	tw := tabwriter.NewWriter(stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSIZE\tCREATED\tRUN\tNAME\tTAGS")
	for _, a := range kept {
		runLabel := a.Run
		if runLabel == "" {
			runLabel = "-"
		}
		name := a.Name
		if a.Pinned {
			name += " *"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			a.ID, spec.FormatSize(a.Size), a.Created.Format("2006-01-02 15:04"),
			runLabel, name, spec.FormatTags(a.Tags))
	}
	tw.Flush()
	fmt.Fprintf(stdout, "%s (* = pinned)\n", plural(len(kept), "artifact"))
	return exitOK
}

func cmdInfo(args []string, stdout, stderr io.Writer) int {
	fs, storeDir := newFlagSet("info", stderr)
	asJSON := fs.Bool("json", false, "emit JSON instead of key: value lines")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		return usageErr(stderr, "info needs exactly one artifact reference")
	}
	st, err := openStore(*storeDir)
	if err != nil {
		return runtimeErr(stderr, err)
	}
	a, err := st.Index.Resolve(fs.Arg(0))
	if err != nil {
		return runtimeErr(stderr, err)
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		enc.Encode(a)
		return exitOK
	}
	fmt.Fprintf(stdout, "id:       %s\n", a.ID)
	fmt.Fprintf(stdout, "name:     %s\n", a.Name)
	fmt.Fprintf(stdout, "digest:   %s\n", a.Digest)
	fmt.Fprintf(stdout, "size:     %s (%d bytes)\n", spec.FormatSize(a.Size), a.Size)
	fmt.Fprintf(stdout, "media:    %s\n", a.Media)
	runLabel := a.Run
	if runLabel == "" {
		runLabel = "-"
	}
	fmt.Fprintf(stdout, "run:      %s\n", runLabel)
	fmt.Fprintf(stdout, "pinned:   %v\n", a.Pinned)
	fmt.Fprintf(stdout, "created:  %s\n", a.Created.Format("2006-01-02T15:04:05Z07:00"))
	fmt.Fprintf(stdout, "tags:     %s\n", spec.FormatTags(a.Tags))
	return exitOK
}

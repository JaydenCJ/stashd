// The retention path: gc, policy, stats, verify.
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/JaydenCJ/stashd/internal/policy"
	"github.com/JaydenCJ/stashd/internal/spec"
)

func cmdGC(args []string, stdout, stderr io.Writer) int {
	fs, storeDir := newFlagSet("gc", stderr)
	dryRun := fs.Bool("dry-run", false, "report what would be expired without deleting anything")
	asJSON := fs.Bool("json", false, "emit the gc result as JSON")
	maxAge := fs.String("max-age", "", "override policy: expire artifacts older than this (e.g. 7d)")
	keepLast := fs.Int("keep-last", 0, "override policy: keep only the newest N artifacts per name")
	maxBytes := fs.String("max-bytes", "", "override policy: physical byte budget (e.g. 2GiB)")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if fs.NArg() != 0 {
		return usageErr(stderr, "gc takes no positional arguments")
	}
	if *keepLast < 0 {
		return usageErr(stderr, "--keep-last must be positive, got %d", *keepLast)
	}
	st, err := openStore(*storeDir)
	if err != nil {
		return runtimeErr(stderr, err)
	}
	var pol *policy.Policy
	if *maxAge != "" || *keepLast > 0 || *maxBytes != "" {
		pol, err = policy.Override(*maxAge, *keepLast, *maxBytes)
		if err != nil {
			return usageErr(stderr, "%v", err)
		}
	} else {
		pol, err = st.LoadPolicy()
		if err != nil {
			return runtimeErr(stderr, err)
		}
	}
	res, err := st.GC(pol, *dryRun)
	if err != nil {
		return runtimeErr(stderr, err)
	}
	if *asJSON {
		if res.Expired == nil {
			res.Expired = []policy.Decision{}
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		enc.Encode(res)
		return exitOK
	}
	verb := "expired"
	if res.DryRun {
		verb = "would expire"
	}
	for _, d := range res.Expired {
		fmt.Fprintf(stdout, "%s %s  %s  [%s] %s\n", verb, d.ID, d.Name, d.Rule, d.Reason)
	}
	if res.PolicyEmpty {
		fmt.Fprintln(stdout, "note: no retention policy installed; only unreferenced blobs are swept")
	}
	summary := "gc:"
	if res.DryRun {
		summary = "gc (dry-run):"
	}
	fmt.Fprintf(stdout, "%s %s %s, %s removed, %s reclaimed\n",
		summary, plural(len(res.Expired), "artifact"), verb,
		plural(res.BlobsRemoved, "blob"), spec.FormatSize(res.BytesReclaimed))
	return exitOK
}

func cmdPolicy(args []string, stdout, stderr io.Writer) int {
	fs, storeDir := newFlagSet("policy", stderr)
	if code, ok := parse(fs, args); !ok {
		return code
	}
	st, err := openStore(*storeDir)
	if err != nil {
		return runtimeErr(stderr, err)
	}
	switch {
	case fs.NArg() == 0 || fs.Arg(0) == "show":
		data, err := os.ReadFile(st.PolicyPath())
		if os.IsNotExist(err) {
			fmt.Fprintln(stdout, "no retention policy installed (stashd gc will only sweep unreferenced blobs)")
			fmt.Fprintf(stdout, "install one with: stashd policy set <file.json>\n")
			return exitOK
		}
		if err != nil {
			return runtimeErr(stderr, err)
		}
		stdout.Write(data)
		return exitOK
	case fs.Arg(0) == "set" && fs.NArg() == 2:
		data, err := os.ReadFile(fs.Arg(1))
		if err != nil {
			return runtimeErr(stderr, err)
		}
		p, err := st.InstallPolicy(data)
		if err != nil {
			return runtimeErr(stderr, err)
		}
		fmt.Fprintf(stdout, "policy installed: %s", plural(len(p.Rules), "rule"))
		if p.MaxTotalBytes != "" {
			fmt.Fprintf(stdout, ", budget %s", p.MaxTotalBytes)
		}
		fmt.Fprintln(stdout)
		return exitOK
	default:
		return usageErr(stderr, "usage: stashd policy [show | set <file.json>]")
	}
}

func cmdStats(args []string, stdout, stderr io.Writer) int {
	fs, storeDir := newFlagSet("stats", stderr)
	asJSON := fs.Bool("json", false, "emit stats as JSON")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	st, err := openStore(*storeDir)
	if err != nil {
		return runtimeErr(stderr, err)
	}
	res, err := st.Stats()
	if err != nil {
		return runtimeErr(stderr, err)
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		enc.Encode(res)
		return exitOK
	}
	fmt.Fprintf(stdout, "store      %s\n", res.Store)
	fmt.Fprintf(stdout, "artifacts  %d (%d pinned)\n", res.Artifacts, res.Pinned)
	fmt.Fprintf(stdout, "runs       %d\n", res.Runs)
	fmt.Fprintf(stdout, "blobs      %d\n", res.Blobs)
	fmt.Fprintf(stdout, "logical    %s\n", spec.FormatSize(res.LogicalBytes))
	fmt.Fprintf(stdout, "physical   %s\n", spec.FormatSize(res.PhysicalBytes))
	fmt.Fprintf(stdout, "dedup      %.2fx (%s saved)\n",
		res.DedupRatio, spec.FormatSize(res.LogicalBytes-res.PhysicalBytes))
	return exitOK
}

func cmdVerify(args []string, stdout, stderr io.Writer) int {
	fs, storeDir := newFlagSet("verify", stderr)
	asJSON := fs.Bool("json", false, "emit the verification report as JSON")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	st, err := openStore(*storeDir)
	if err != nil {
		return runtimeErr(stderr, err)
	}
	res, err := st.Verify()
	if err != nil {
		return runtimeErr(stderr, err)
	}
	if *asJSON {
		if res.Corrupt == nil {
			res.Corrupt = []string{}
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		enc.Encode(res)
	} else {
		for _, d := range res.Corrupt {
			fmt.Fprintf(stdout, "CORRUPT  %s\n", d)
		}
		for _, m := range res.Missing {
			fmt.Fprintf(stdout, "MISSING  %s  %s (%s)\n", m.ID, m.Name, m.Digest)
		}
		fmt.Fprintf(stdout, "verify: %s checked, %d corrupt, %d missing, %s\n",
			plural(res.BlobsChecked, "blob"), len(res.Corrupt), len(res.Missing),
			plural(res.Orphans, "orphan"))
	}
	if !res.OK() {
		return exitFailed
	}
	return exitOK
}

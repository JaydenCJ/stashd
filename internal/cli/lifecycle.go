// The lifecycle path: tag, untag, pin, unpin, rm.
package cli

import (
	"fmt"
	"io"

	"github.com/JaydenCJ/stashd/internal/index"
	"github.com/JaydenCJ/stashd/internal/spec"
)

func cmdTag(args []string, stdout, stderr io.Writer) int {
	fs, storeDir := newFlagSet("tag", stderr)
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if fs.NArg() < 2 {
		return usageErr(stderr, "tag needs an artifact reference and at least one key=value")
	}
	pairs := fs.Args()[1:]
	type kv struct{ k, v string }
	parsed := make([]kv, 0, len(pairs))
	for _, p := range pairs {
		k, v, err := spec.ParseTag(p)
		if err != nil {
			return usageErr(stderr, "%v", err)
		}
		parsed = append(parsed, kv{k, v})
	}
	st, err := openStore(*storeDir)
	if err != nil {
		return runtimeErr(stderr, err)
	}
	a, err := st.Update(fs.Arg(0), func(a *index.Artifact) error {
		if a.Tags == nil {
			a.Tags = map[string]string{}
		}
		for _, p := range parsed {
			a.Tags[p.k] = p.v
		}
		return nil
	})
	if err != nil {
		return runtimeErr(stderr, err)
	}
	fmt.Fprintf(stdout, "%s  tags: %s\n", a.ID, spec.FormatTags(a.Tags))
	return exitOK
}

func cmdUntag(args []string, stdout, stderr io.Writer) int {
	fs, storeDir := newFlagSet("untag", stderr)
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if fs.NArg() < 2 {
		return usageErr(stderr, "untag needs an artifact reference and at least one key")
	}
	st, err := openStore(*storeDir)
	if err != nil {
		return runtimeErr(stderr, err)
	}
	keys := fs.Args()[1:]
	a, err := st.Update(fs.Arg(0), func(a *index.Artifact) error {
		for _, k := range keys {
			if _, ok := a.Tags[k]; !ok {
				return fmt.Errorf("artifact %s has no tag %q", a.ID, k)
			}
			delete(a.Tags, k)
		}
		return nil
	})
	if err != nil {
		return runtimeErr(stderr, err)
	}
	fmt.Fprintf(stdout, "%s  tags: %s\n", a.ID, spec.FormatTags(a.Tags))
	return exitOK
}

func cmdPin(args []string, stdout, stderr io.Writer, pinned bool) int {
	verb := "pin"
	if !pinned {
		verb = "unpin"
	}
	fs, storeDir := newFlagSet(verb, stderr)
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		return usageErr(stderr, "%s needs exactly one artifact reference", verb)
	}
	st, err := openStore(*storeDir)
	if err != nil {
		return runtimeErr(stderr, err)
	}
	a, err := st.Update(fs.Arg(0), func(a *index.Artifact) error {
		a.Pinned = pinned
		return nil
	})
	if err != nil {
		return runtimeErr(stderr, err)
	}
	if pinned {
		fmt.Fprintf(stdout, "pinned %s (%s) — retention will never expire it\n", a.ID, a.Name)
	} else {
		fmt.Fprintf(stdout, "unpinned %s (%s)\n", a.ID, a.Name)
	}
	return exitOK
}

func cmdRm(args []string, stdout, stderr io.Writer) int {
	fs, storeDir := newFlagSet("rm", stderr)
	force := fs.Bool("force", false, "remove even if pinned")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		return usageErr(stderr, "rm needs exactly one artifact reference")
	}
	st, err := openStore(*storeDir)
	if err != nil {
		return runtimeErr(stderr, err)
	}
	res, err := st.Remove(fs.Arg(0), *force)
	if err != nil {
		return runtimeErr(stderr, err)
	}
	if res.BlobRemoved {
		fmt.Fprintf(stdout, "removed %s (%s) — blob freed, %s reclaimed\n",
			res.Artifact.ID, res.Artifact.Name, spec.FormatSize(res.BytesFreed))
	} else {
		fmt.Fprintf(stdout, "removed %s (%s) — blob kept, %s\n",
			res.Artifact.ID, res.Artifact.Name, plural(res.OtherRefCount, "other reference"))
	}
	return exitOK
}

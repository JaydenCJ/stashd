// The write path: `stashd put` and `stashd get`.
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/JaydenCJ/stashd/internal/spec"
	"github.com/JaydenCJ/stashd/internal/store"
)

func cmdPut(args []string, stdout, stderr io.Writer) int {
	fs, storeDir := newFlagSet("put", stderr)
	name := fs.String("name", "", "artifact name (default: base name of the file; required for stdin)")
	media := fs.String("type", "", "media type (default: sniffed from the name's extension)")
	run := fs.String("run", "", "run id this artifact belongs to (default $STASHD_RUN)")
	pin := fs.Bool("pin", false, "pin the artifact so retention never expires it")
	quiet := fs.Bool("q", false, "print only the artifact id")
	asJSON := fs.Bool("json", false, "print the full artifact record as JSON")
	var tags repeatedFlag
	fs.Var(&tags, "tag", "key=value tag (repeatable)")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		return usageErr(stderr, "put needs exactly one file argument (or '-' for stdin)")
	}

	src := fs.Arg(0)
	var in io.Reader
	if src == "-" {
		in = os.Stdin
		if *name == "" {
			return usageErr(stderr, "put from stdin requires --name")
		}
	} else {
		f, err := os.Open(src)
		if err != nil {
			return runtimeErr(stderr, err)
		}
		defer f.Close()
		in = f
		if *name == "" {
			*name = filepath.Base(src)
		}
	}
	if *run == "" {
		*run = os.Getenv("STASHD_RUN")
	}
	tagMap := map[string]string{}
	for _, t := range tags {
		k, v, err := spec.ParseTag(t)
		if err != nil {
			return usageErr(stderr, "%v", err)
		}
		tagMap[k] = v
	}

	st, err := openStore(*storeDir)
	if err != nil {
		return runtimeErr(stderr, err)
	}
	a, deduped, err := st.Put(in, store.PutOptions{
		Name: *name, Media: *media, Run: *run, Tags: tagMap, Pinned: *pin,
	})
	if err != nil {
		return runtimeErr(stderr, err)
	}

	switch {
	case *quiet:
		fmt.Fprintln(stdout, a.ID)
	case *asJSON:
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		enc.Encode(a)
	default:
		note := "new blob"
		if deduped {
			note = "dedup: blob already stored"
		}
		fmt.Fprintf(stdout, "%s  %s  %s  %s  (%s)\n",
			a.ID, shortDigest(a.Digest), spec.FormatSize(a.Size), a.Name, note)
	}
	return exitOK
}

func cmdGet(args []string, stdout, stderr io.Writer) int {
	fs, storeDir := newFlagSet("get", stderr)
	out := fs.String("o", "", "write content to this file instead of stdout")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		return usageErr(stderr, "get needs exactly one artifact reference")
	}
	st, err := openStore(*storeDir)
	if err != nil {
		return runtimeErr(stderr, err)
	}
	a, rc, err := st.Get(fs.Arg(0))
	if err != nil {
		return runtimeErr(stderr, err)
	}
	defer rc.Close()

	var dst io.Writer = stdout
	var outFile *os.File
	if *out != "" {
		outFile, err = os.Create(*out)
		if err != nil {
			return runtimeErr(stderr, err)
		}
		dst = outFile
	}
	if _, err := io.Copy(dst, rc); err != nil {
		if outFile != nil {
			outFile.Close()
		}
		// The verifying reader turns silent bit-rot into a loud failure.
		fmt.Fprintf(stderr, "stashd: %v\n", err)
		return exitFailed
	}
	if outFile != nil {
		// A close error (e.g. disk full on the final flush) must not
		// masquerade as a successful write.
		if err := outFile.Close(); err != nil {
			return runtimeErr(stderr, err)
		}
		fmt.Fprintf(stderr, "wrote %s (%s) to %s\n", a.Name, spec.FormatSize(a.Size), *out)
	}
	return exitOK
}

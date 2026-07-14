// Package cli implements the stashd command-line interface. Every command
// is a small function over the store package; all output goes through the
// provided writers so the whole CLI is testable in-process.
package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/JaydenCJ/stashd/internal/store"
	"github.com/JaydenCJ/stashd/internal/version"
)

// Exit codes, kept boring on purpose.
const (
	exitOK      = 0
	exitFailed  = 1 // verify found corruption, get hit a bad blob
	exitUsage   = 2
	exitRuntime = 3
)

const usageText = `stashd — content-addressed artifact store for agent outputs

Usage:
  stashd <command> [flags] [args]

Commands:
  put      store a file (or stdin) and print its artifact id
  get      stream an artifact's content to stdout or a file
  info     show one artifact's metadata
  ls       list artifacts, filterable by tag, run, and name glob
  tag      add or update key=value tags on an artifact
  untag    remove tags from an artifact
  pin      protect an artifact from every retention rule
  unpin    remove that protection
  rm       delete an artifact record (blob freed when unreferenced)
  gc       apply the retention policy and sweep unreferenced blobs
  policy   show or install the retention policy
  stats    store totals and dedup ratio
  verify   re-hash every blob and check all references
  version  print the stashd version

Every command accepts --store PATH (default: $STASHD_DIR, else ~/.stashd).
Run 'stashd <command> -h' for command flags.
`

// Run executes the CLI and returns the process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return exitUsage
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "stashd %s\n", version.Version)
		return exitOK
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usageText)
		return exitOK
	case "put":
		return cmdPut(rest, stdout, stderr)
	case "get":
		return cmdGet(rest, stdout, stderr)
	case "info":
		return cmdInfo(rest, stdout, stderr)
	case "ls":
		return cmdLs(rest, stdout, stderr)
	case "tag":
		return cmdTag(rest, stdout, stderr)
	case "untag":
		return cmdUntag(rest, stdout, stderr)
	case "pin":
		return cmdPin(rest, stdout, stderr, true)
	case "unpin":
		return cmdPin(rest, stdout, stderr, false)
	case "rm":
		return cmdRm(rest, stdout, stderr)
	case "gc":
		return cmdGC(rest, stdout, stderr)
	case "policy":
		return cmdPolicy(rest, stdout, stderr)
	case "stats":
		return cmdStats(rest, stdout, stderr)
	case "verify":
		return cmdVerify(rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "stashd: unknown command %q\n\n%s", cmd, usageText)
		return exitUsage
	}
}

// newFlagSet builds a per-command flag set wired for testability, plus the
// shared --store flag every command carries.
func newFlagSet(name string, stderr io.Writer) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	storeDir := fs.String("store", "", "store directory (default $STASHD_DIR, else ~/.stashd)")
	return fs, storeDir
}

// openStore resolves the store directory: --store flag, then $STASHD_DIR,
// then ~/.stashd.
func openStore(flagValue string) (*store.Store, error) {
	dir := flagValue
	if dir == "" {
		dir = os.Getenv("STASHD_DIR")
	}
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("cannot resolve home directory: %w (pass --store)", err)
		}
		dir = filepath.Join(home, ".stashd")
	}
	return store.Open(dir)
}

// runtimeErr prints a runtime failure and returns its exit code.
func runtimeErr(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "stashd: %v\n", err)
	return exitRuntime
}

// usageErr prints a usage failure and returns its exit code.
func usageErr(stderr io.Writer, format string, args ...any) int {
	fmt.Fprintf(stderr, "stashd: "+format+"\n", args...)
	return exitUsage
}

// parse runs fs over args, translating flag errors to the usage exit code.
// The bool result reports whether parsing succeeded.
func parse(fs *flag.FlagSet, args []string) (int, bool) {
	if err := fs.Parse(args); err != nil {
		return exitUsage, false
	}
	return exitOK, true
}

// plural renders "<n> <noun>" with a trailing "s" when n != 1, because
// nobody should have to read "1 artifacts".
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// shortDigest abbreviates "sha256:<64 hex>" for human-facing lines.
func shortDigest(digest string) string {
	h := strings.TrimPrefix(digest, "sha256:")
	if len(h) > 12 {
		h = h[:12]
	}
	return "sha256:" + h + "…"
}

// repeatedFlag collects a flag given multiple times (e.g. --tag k=v).
type repeatedFlag []string

func (r *repeatedFlag) String() string { return strings.Join(*r, ",") }

func (r *repeatedFlag) Set(v string) error {
	*r = append(*r, v)
	return nil
}

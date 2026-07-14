// In-process CLI integration tests: every command is exercised through
// Run() against a temp store, asserting on real output and exit codes.
// No subprocesses, no network, no wall-clock dependence — artifacts are
// aged by seeding the store with an injected clock.
package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JaydenCJ/stashd/internal/index"
	"github.com/JaydenCJ/stashd/internal/store"
)

// run invokes the CLI in-process and captures everything.
func run(args ...string) (code int, stdout, stderr string) {
	var out, errBuf bytes.Buffer
	code = Run(args, &out, &errBuf)
	return code, out.String(), errBuf.String()
}

// seed writes an artifact directly through the store with an injected
// clock set `age` in the past, so CLI retention tests control artifact
// age exactly without sleeping. Age thresholds in the tests leave hours
// of margin, so the milliseconds a test run takes cannot flip a result.
func seed(t *testing.T, dir, name, content string, age time.Duration, opt store.PutOptions) *index.Artifact {
	t.Helper()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	created := time.Now().Add(-age)
	s.Now = func() time.Time { return created }
	opt.Name = name
	a, _, err := s.Put(strings.NewReader(content), opt)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestVersionCommand(t *testing.T) {
	code, out, _ := run("version")
	if code != 0 || out != "stashd 0.1.0\n" {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestUsageErrorsExit2(t *testing.T) {
	code, _, errOut := run()
	if code != 2 || !strings.Contains(errOut, "Usage:") {
		t.Fatalf("no args: code=%d err=%q", code, errOut)
	}
	code, _, errOut = run("frobnicate")
	if code != 2 || !strings.Contains(errOut, `unknown command "frobnicate"`) {
		t.Fatalf("unknown command: code=%d err=%q", code, errOut)
	}
	if code, _, _ := run("ls", "--store", t.TempDir(), "--not-a-flag"); code != 2 {
		t.Fatalf("bad flag: code=%d", code)
	}
}

func TestPutPrintsIDDigestAndDedupNote(t *testing.T) {
	dir := t.TempDir()
	f := writeFile(t, t.TempDir(), "report.md", "# findings\n")
	code, out, errOut := run("put", "--store", dir, f)
	if code != 0 {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	if !strings.Contains(out, "report.md") || !strings.Contains(out, "(new blob)") ||
		!strings.Contains(out, "sha256:") {
		t.Fatalf("out=%q", out)
	}
	// Same content again: the CLI must say the blob was deduplicated.
	code, out, _ = run("put", "--store", dir, f)
	if code != 0 || !strings.Contains(out, "dedup: blob already stored") {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestPutQuietPrintsBareID(t *testing.T) {
	dir := t.TempDir()
	f := writeFile(t, t.TempDir(), "x.txt", "quiet")
	code, out, _ := run("put", "--store", dir, "-q", f)
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	id := strings.TrimSpace(out)
	if len(id) != 12 || strings.ContainsAny(out, " (") {
		t.Fatalf("quiet output should be a bare 12-char id: %q", out)
	}
	// The bare id must be usable as a reference immediately.
	code, out, _ = run("get", "--store", dir, id)
	if code != 0 || out != "quiet" {
		t.Fatalf("get by quiet id: code=%d out=%q", code, out)
	}
}

func TestPutUsageErrors(t *testing.T) {
	dir := t.TempDir()
	// stdin without --name would create an unfindable artifact.
	code, _, errOut := run("put", "--store", dir, "-")
	if code != 2 || !strings.Contains(errOut, "--name") {
		t.Fatalf("stdin: code=%d err=%q", code, errOut)
	}
	f := writeFile(t, t.TempDir(), "x.txt", "x")
	code, _, errOut = run("put", "--store", dir, "--tag", "notapair", f)
	if code != 2 || !strings.Contains(errOut, "key=value") {
		t.Fatalf("bad tag: code=%d err=%q", code, errOut)
	}
}

func TestPutHonorsRunEnvFallback(t *testing.T) {
	dir := t.TempDir()
	f := writeFile(t, t.TempDir(), "x.txt", "env run")
	t.Setenv("STASHD_RUN", "run-from-env")
	code, out, _ := run("put", "--store", dir, "-q", f)
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	_, infoOut, _ := run("info", "--store", dir, strings.TrimSpace(out))
	if !strings.Contains(infoOut, "run:      run-from-env") {
		t.Fatalf("info=%q", infoOut)
	}
}

func TestGetWritesToFileWithO(t *testing.T) {
	dir := t.TempDir()
	a := seed(t, dir, "diff.patch", "--- a\n+++ b\n", 0, store.PutOptions{})
	dst := filepath.Join(t.TempDir(), "out.patch")
	code, _, _ := run("get", "--store", dir, "-o", dst, a.ID)
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	data, err := os.ReadFile(dst)
	if err != nil || string(data) != "--- a\n+++ b\n" {
		t.Fatalf("got %q, %v", data, err)
	}
	// An unknown reference is a runtime error, not silence.
	code, _, errOut := run("get", "--store", dir, "cafecafecafe")
	if code != 3 || !strings.Contains(errOut, "no artifact matches") {
		t.Fatalf("unknown ref: code=%d err=%q", code, errOut)
	}
}

func TestInfoTextAndJSONAgree(t *testing.T) {
	dir := t.TempDir()
	a := seed(t, dir, "shot.png", "png bytes", 0, store.PutOptions{
		Run: "run-3", Tags: map[string]string{"step": "login"},
	})
	code, out, _ := run("info", "--store", dir, a.ID)
	if code != 0 || !strings.Contains(out, "media:    image/png") ||
		!strings.Contains(out, "step=login") {
		t.Fatalf("code=%d out=%q", code, out)
	}
	code, out, _ = run("info", "--store", dir, "--json", a.ID)
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	var got index.Artifact
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.ID != a.ID || got.Run != "run-3" {
		t.Fatalf("json info: %q, %v", out, err)
	}
}

func TestLsFiltersByTagRunAndName(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, "a.png", "1", 3*time.Hour, store.PutOptions{Run: "run-1", Tags: map[string]string{"kind": "shot"}})
	seed(t, dir, "b.png", "2", 2*time.Hour, store.PutOptions{Run: "run-2", Tags: map[string]string{"kind": "shot"}})
	seed(t, dir, "c.log", "3", 1*time.Hour, store.PutOptions{Run: "run-2"})

	code, out, _ := run("ls", "--store", dir, "--tag", "kind=shot")
	if code != 0 || strings.Contains(out, "c.log") || !strings.Contains(out, "2 artifacts") {
		t.Fatalf("tag filter: %q", out)
	}
	_, out, _ = run("ls", "--store", dir, "--run", "run-2")
	if strings.Contains(out, "a.png") || !strings.Contains(out, "c.log") {
		t.Fatalf("run filter: %q", out)
	}
	_, out, _ = run("ls", "--store", dir, "--name", "*.png")
	if strings.Contains(out, "c.log") || !strings.Contains(out, "b.png") {
		t.Fatalf("name filter: %q", out)
	}
}

func TestLsJSONIsNewestFirstArray(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, "old.txt", "1", 5*time.Hour, store.PutOptions{})
	seed(t, dir, "new.txt", "2", 1*time.Hour, store.PutOptions{})
	code, out, _ := run("ls", "--store", dir, "--json")
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	var arts []index.Artifact
	if err := json.Unmarshal([]byte(out), &arts); err != nil || len(arts) != 2 {
		t.Fatalf("json ls: %v, %v", arts, err)
	}
	if arts[0].Name != "new.txt" || arts[1].Name != "old.txt" {
		t.Fatalf("order wrong: %s, %s", arts[0].Name, arts[1].Name)
	}
}

func TestLsEmptyStore(t *testing.T) {
	dir := t.TempDir()
	code, out, _ := run("ls", "--store", dir)
	if code != 0 || !strings.Contains(out, "no artifacts") {
		t.Fatalf("code=%d out=%q", code, out)
	}
	// JSON mode must emit a valid empty array, not "null".
	_, out, _ = run("ls", "--store", dir, "--json")
	if strings.TrimSpace(out) != "[]" {
		t.Fatalf("empty json ls = %q", out)
	}
}

func TestTagUntagLifecycle(t *testing.T) {
	dir := t.TempDir()
	a := seed(t, dir, "x.txt", "x", 0, store.PutOptions{})
	code, out, _ := run("tag", "--store", dir, a.ID, "phase=review", "owner=qa")
	if code != 0 || !strings.Contains(out, "owner=qa,phase=review") {
		t.Fatalf("tag: code=%d out=%q", code, out)
	}
	code, out, _ = run("untag", "--store", dir, a.ID, "owner")
	if code != 0 || strings.Contains(out, "owner=") || !strings.Contains(out, "phase=review") {
		t.Fatalf("untag: code=%d out=%q", code, out)
	}
	code, _, errOut := run("untag", "--store", dir, a.ID, "nope")
	if code != 3 || !strings.Contains(errOut, `no tag "nope"`) {
		t.Fatalf("untag missing: code=%d err=%q", code, errOut)
	}
}

func TestPinBlocksRmUntilForceOrUnpin(t *testing.T) {
	dir := t.TempDir()
	a := seed(t, dir, "keep.md", "precious", 0, store.PutOptions{})
	if code, _, _ := run("pin", "--store", dir, a.ID); code != 0 {
		t.Fatal("pin failed")
	}
	code, _, errOut := run("rm", "--store", dir, a.ID)
	if code != 3 || !strings.Contains(errOut, "pinned") {
		t.Fatalf("rm pinned: code=%d err=%q", code, errOut)
	}
	if code, _, _ := run("unpin", "--store", dir, a.ID); code != 0 {
		t.Fatal("unpin failed")
	}
	code, out, _ := run("rm", "--store", dir, a.ID)
	if code != 0 || !strings.Contains(out, "blob freed") {
		t.Fatalf("rm after unpin: code=%d out=%q", code, out)
	}
}

func TestRmKeepsSharedBlob(t *testing.T) {
	dir := t.TempDir()
	a := seed(t, dir, "a.txt", "shared payload", 0, store.PutOptions{})
	seed(t, dir, "b.txt", "shared payload", 0, store.PutOptions{})
	code, out, _ := run("rm", "--store", dir, a.ID)
	if code != 0 || !strings.Contains(out, "blob kept, 1 other reference") {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestGCMaxAgeOverrideExpiresOldArtifacts(t *testing.T) {
	dir := t.TempDir()
	old := seed(t, dir, "old.log", "old", 2000*time.Hour, store.PutOptions{})
	seed(t, dir, "new.log", "new", 1*time.Hour, store.PutOptions{})
	code, out, _ := run("gc", "--store", dir, "--dry-run", "--max-age", "1000h")
	if code != 0 || !strings.Contains(out, old.ID) || strings.Contains(out, "new.log") {
		t.Fatalf("dry-run plan wrong: code=%d out=%q", code, out)
	}
	if !strings.Contains(out, "gc (dry-run):") {
		t.Fatalf("missing dry-run summary: %q", out)
	}
	code, out, _ = run("gc", "--store", dir, "--max-age", "1000h")
	if code != 0 || !strings.Contains(out, `[rule "cli-override"]`) ||
		!strings.Contains(out, "1 blob removed") {
		t.Fatalf("real gc: code=%d out=%q", code, out)
	}
	// The old artifact is gone; the fresh one survived.
	if code, _, _ := run("info", "--store", dir, old.ID); code != 3 {
		t.Fatal("expired artifact still resolvable")
	}
	_, lsOut, _ := run("ls", "--store", dir)
	if strings.Contains(lsOut, "old.log") || !strings.Contains(lsOut, "new.log") {
		t.Fatalf("wrong survivor set: %q", lsOut)
	}
}

func TestGCUsesInstalledPolicy(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, "s1.png", "shot one", 30*time.Hour, store.PutOptions{Tags: map[string]string{"kind": "shot"}})
	seed(t, dir, "s2.png", "shot two", 1*time.Hour, store.PutOptions{Tags: map[string]string{"kind": "shot"}})
	polFile := writeFile(t, t.TempDir(), "policy.json",
		`{"rules": [{"name": "shots", "match": {"tags": {"kind": "shot"}}, "keep_last": 1, "group_by": "run"}]}`)
	code, out, _ := run("policy", "--store", dir, "set", polFile)
	if code != 0 || !strings.Contains(out, "policy installed: 1 rule\n") {
		t.Fatalf("policy set: code=%d out=%q", code, out)
	}
	code, out, _ = run("policy", "--store", dir, "show")
	if code != 0 || !strings.Contains(out, `"keep_last": 1`) {
		t.Fatalf("policy show: %q", out)
	}
	code, out, _ = run("gc", "--store", dir)
	if code != 0 || !strings.Contains(out, "s1.png") || !strings.Contains(out, "[rule \"shots\"]") {
		t.Fatalf("gc via policy: code=%d out=%q", code, out)
	}
	_, lsOut, _ := run("ls", "--store", dir)
	if strings.Contains(lsOut, "s1.png") || !strings.Contains(lsOut, "s2.png") {
		t.Fatalf("wrong survivor: %q", lsOut)
	}
}

func TestGCWithoutPolicyNotesAndSweeps(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, "x.txt", "x", 0, store.PutOptions{})
	code, out, _ := run("gc", "--store", dir)
	if code != 0 || !strings.Contains(out, "no retention policy installed") ||
		!strings.Contains(out, "0 artifacts expired") {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestGCRejectsBadOverride(t *testing.T) {
	dir := t.TempDir()
	code, _, errOut := run("gc", "--store", dir, "--max-age", "banana")
	if code != 2 || !strings.Contains(errOut, "invalid duration") {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
}

func TestGCRejectsNegativeKeepLast(t *testing.T) {
	// A negative --keep-last must be a loud usage error; silently falling
	// back to the installed policy could delete far more than intended.
	dir := t.TempDir()
	code, _, errOut := run("gc", "--store", dir, "--keep-last", "-2")
	if code != 2 || !strings.Contains(errOut, "--keep-last must be positive") {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
}

func TestPolicySetRejectsInvalidFile(t *testing.T) {
	dir := t.TempDir()
	polFile := writeFile(t, t.TempDir(), "bad.json", `{"rules": [{"max_agee": "7d"}]}`)
	code, _, errOut := run("policy", "--store", dir, "set", polFile)
	if code != 3 || !strings.Contains(errOut, "max_agee") {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	// Nothing must have been installed.
	_, out, _ := run("policy", "--store", dir, "show")
	if !strings.Contains(out, "no retention policy installed") {
		t.Fatalf("show=%q", out)
	}
}

func TestStatsReportsDedup(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, "a.txt", strings.Repeat("z", 1000), 0, store.PutOptions{})
	seed(t, dir, "b.txt", strings.Repeat("z", 1000), 0, store.PutOptions{})
	code, out, _ := run("stats", "--store", dir)
	if code != 0 || !strings.Contains(out, "artifacts  2") ||
		!strings.Contains(out, "blobs      1") || !strings.Contains(out, "dedup      2.00x") {
		t.Fatalf("code=%d out=%q", code, out)
	}
	code, out, _ = run("stats", "--store", dir, "--json")
	if code != 0 || !strings.Contains(out, `"dedup_ratio": 2`) {
		t.Fatalf("json stats: %q", out)
	}
}

func TestVerifyReportsHealthThroughExitCode(t *testing.T) {
	dir := t.TempDir()
	a := seed(t, dir, "rot.txt", "will rot away", 0, store.PutOptions{})
	code, out, _ := run("verify", "--store", dir)
	if code != 0 || !strings.Contains(out, "1 blob checked, 0 corrupt, 0 missing, 0 orphans") {
		t.Fatalf("healthy: code=%d out=%q", code, out)
	}
	// Corrupt the blob on disk; verify must flag it and exit 1.
	h := strings.TrimPrefix(a.Digest, "sha256:")
	p := filepath.Join(dir, "objects", "sha256", h[:2], h[2:])
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("rotted bytes!"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ = run("verify", "--store", dir)
	if code != 1 || !strings.Contains(out, "CORRUPT") {
		t.Fatalf("corrupt: code=%d out=%q", code, out)
	}
}

func TestStoreDirEnvFallback(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("STASHD_DIR", dir)
	f := writeFile(t, t.TempDir(), "env.txt", "via env")
	code, out, _ := run("put", "-q", f)
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	code, got, _ := run("get", strings.TrimSpace(out))
	if code != 0 || got != "via env" {
		t.Fatalf("code=%d got=%q", code, got)
	}
}

func TestResolveByDigestPrefix(t *testing.T) {
	dir := t.TempDir()
	a := seed(t, dir, "unique.txt", "one of a kind", 0, store.PutOptions{})
	h := strings.TrimPrefix(a.Digest, "sha256:")
	code, out, _ := run("get", "--store", dir, h[:10])
	if code != 0 || out != "one of a kind" {
		t.Fatalf("digest-prefix get: code=%d out=%q", code, out)
	}
}

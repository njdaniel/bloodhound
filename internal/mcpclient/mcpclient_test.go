package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/njdaniel/bloodhound/internal/llm"
)

var update = flag.Bool("update", false, "rewrite golden files")

// Fake server binary, built once on first use and cleaned up in TestMain.
var (
	buildOnce sync.Once
	buildDir  string
	buildErr  error
	fakeBin   string
)

func TestMain(m *testing.M) {
	code := m.Run()
	if buildDir != "" {
		os.RemoveAll(buildDir)
	}
	os.Exit(code)
}

// fakeServer compiles testdata/fakeserver once and returns the binary path.
// Tests that need it are skipped in -short mode, mirroring the mcp-prom
// conformance test.
func fakeServer(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping child-process MCP tests in -short mode")
	}
	buildOnce.Do(func() {
		buildDir, buildErr = os.MkdirTemp("", "mcpclient-fakeserver")
		if buildErr != nil {
			return
		}
		fakeBin = filepath.Join(buildDir, "fakeserver")
		out, err := exec.Command("go", "build", "-o", fakeBin, "./testdata/fakeserver").CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("building fakeserver: %w\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return fakeBin
}

// newSession connects to a fresh fake server with captures under a temp dir.
// The session is closed and the context cancelled at test cleanup.
func newSession(t *testing.T, cfg Config) (*Session, string) {
	t.Helper()
	bin := fakeServer(t)
	cfg.Command = bin
	if cfg.CaptureDir == "" {
		cfg.CaptureDir = t.TempDir()
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s, err := Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, cfg.CaptureDir
}

// wantTools maps every fakeserver tool to its input schema, as the client
// should see it after listing.
var wantTools = map[string]string{
	"echo":  `{"type":"object","required":["text"],"additionalProperties":false,"properties":{"text":{"type":"string"}}}`,
	"fail":  `{"type":"object","required":["message"],"additionalProperties":false,"properties":{"message":{"type":"string"}}}`,
	"sleep": `{"type":"object","required":["ms"],"additionalProperties":false,"properties":{"ms":{"type":"integer","minimum":0}}}`,
}

func TestToolsListsServerTools(t *testing.T) {
	s, _ := newSession(t, Config{})
	defs, err := s.Tools(context.Background())
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(defs) != len(wantTools) {
		t.Errorf("listed %d tools, want %d", len(defs), len(wantTools))
	}
	byName := map[string]ToolDef{}
	for _, d := range defs {
		byName[d.Name] = d
	}
	for name, wantRaw := range wantTools {
		def, ok := byName[name]
		if !ok {
			t.Errorf("tool %q not listed", name)
			continue
		}
		if def.Description == "" {
			t.Errorf("tool %q has no description", name)
		}
		var want, got any
		if err := json.Unmarshal([]byte(wantRaw), &want); err != nil {
			t.Fatalf("parsing expected schema for %q: %v", name, err)
		}
		if err := json.Unmarshal(def.InputSchema, &got); err != nil {
			t.Fatalf("parsing listed schema for %q: %v", name, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("tool %q schema differs:\n got: %s\nwant: %s", name, def.InputSchema, wantRaw)
		}
	}

	// ToolDef is the source of truth for hound tool definitions and must
	// stay field-for-field convertible to llm.Tool.
	lt := llm.Tool(byName["echo"])
	if lt.Name != "echo" || lt.Description == "" || len(lt.InputSchema) == 0 {
		t.Errorf("llm.Tool conversion lost data: %+v", lt)
	}
}

func TestCallToolHappyPathAndCaptureGolden(t *testing.T) {
	s, dir := newSession(t, Config{})
	res, err := s.CallTool(context.Background(), "echo", json.RawMessage(`{"text":"hello"}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.Text != "echo: hello" {
		t.Errorf("Text = %q, want %q", res.Text, "echo: hello")
	}
	if res.IsError {
		t.Error("IsError = true for a successful call")
	}
	if res.CaptureRef != "mcp/000-echo.json" {
		t.Errorf("CaptureRef = %q, want %q", res.CaptureRef, "mcp/000-echo.json")
	}

	got, err := os.ReadFile(filepath.Join(dir, "mcp", "000-echo.json"))
	if err != nil {
		t.Fatalf("reading capture file: %v", err)
	}
	goldenPath := filepath.Join("testdata", "capture.golden.json")
	if *update {
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatalf("writing golden: %v", err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("reading golden (run with -update to regenerate): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("capture file differs from golden.\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestToolErrorReturnedAsValue(t *testing.T) {
	s, dir := newSession(t, Config{})
	res, err := s.CallTool(context.Background(), "fail", json.RawMessage(`{"message":"bad promql"}`))
	if err != nil {
		t.Fatalf("tool error surfaced as Go error: %v", err)
	}
	if !res.IsError {
		t.Fatal("IsError = false for a failing tool")
	}
	if !strings.Contains(res.Text, "bad promql") {
		t.Errorf("Text = %q, want it to carry the tool's message", res.Text)
	}

	// The failed call is still evidence: captured, with is_error recorded.
	data, err := os.ReadFile(filepath.Join(dir, "mcp", "000-fail.json"))
	if err != nil {
		t.Fatalf("reading capture file: %v", err)
	}
	var rec CaptureRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("unmarshaling capture record: %v", err)
	}
	if rec.Result == nil || !rec.Result.IsError {
		t.Errorf("capture record does not mark the tool error: %+v", rec)
	}
	if rec.Error != "" {
		t.Errorf("capture record has a transport error for a tool error: %q", rec.Error)
	}
}

func TestUnknownToolIsTransportError(t *testing.T) {
	s, _ := newSession(t, Config{})
	_, err := s.CallTool(context.Background(), "no_such_tool", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("calling an unknown tool returned nil error")
	}
}

func TestPerCallTimeout(t *testing.T) {
	s, dir := newSession(t, Config{CallTimeout: 150 * time.Millisecond})
	start := time.Now()
	_, err := s.CallTool(context.Background(), "sleep", json.RawMessage(`{"ms":30000}`))
	if err == nil {
		t.Fatal("timed-out call returned nil error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded in the chain", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("call took %v, timeout did not bound it", elapsed)
	}

	// The timeout is a transport error: captured with error, no result.
	data, err := os.ReadFile(filepath.Join(dir, "mcp", "000-sleep.json"))
	if err != nil {
		t.Fatalf("reading capture file: %v", err)
	}
	var rec CaptureRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("unmarshaling capture record: %v", err)
	}
	if rec.Error == "" {
		t.Error("capture record has no error for a timed-out call")
	}
	if rec.Result != nil {
		t.Errorf("capture record has a result for a timed-out call: %+v", rec.Result)
	}
}

func TestKillMidCallSurfacesTransportError(t *testing.T) {
	s, _ := newSession(t, Config{})
	errc := make(chan error, 1)
	go func() {
		_, err := s.CallTool(context.Background(), "sleep", json.RawMessage(`{"ms":60000}`))
		errc <- err
	}()
	time.Sleep(200 * time.Millisecond) // let the call get in flight
	if err := s.cmd.Process.Kill(); err != nil {
		t.Fatalf("killing child: %v", err)
	}
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("call against a killed server returned nil error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("call against a killed server hung")
	}
}

// processGone reports whether pid no longer exists. A zombie (exited but
// unreaped) still answers signal 0, so it counts as present — which is
// exactly what the shutdown tests need to detect.
func processGone(pid int) bool {
	err := syscall.Kill(pid, 0)
	return errors.Is(err, syscall.ESRCH)
}

func TestContextCancelKillsChild(t *testing.T) {
	bin := fakeServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	s, err := Connect(ctx, Config{Command: bin, CaptureDir: t.TempDir()})
	if err != nil {
		cancel()
		t.Fatalf("Connect: %v", err)
	}
	pid := s.cmd.Process.Pid
	if processGone(pid) {
		t.Fatalf("child pid %d not running after Connect", pid)
	}

	cancel()

	deadline := time.Now().Add(15 * time.Second)
	for !processGone(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("child pid %d still exists after ctx cancel (zombie or running)", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Close after the watcher already closed must be safe.
	if err := s.Close(); err != nil {
		t.Logf("Close after cancel: %v (tolerated: session already shut down)", err)
	}
}

func TestCloseKillsChild(t *testing.T) {
	s, _ := newSession(t, Config{})
	pid := s.cmd.Process.Pid
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for !processGone(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("child pid %d still exists after Close", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// The two tests below check this stream end to end. Both halves of the
// filename — seqname.Render and the seqname.Prefix that Next parses it back
// with — now live once in internal/seqname, shared with
// internal/llm/middleware so the two capture streams cannot resume with
// different numbering (spec 002 §4.3). These stay local anyway: they are the
// only place the renderer and the parser meet through this package's real
// write path, on real files on disk, rather than through seqname's own
// round-trip test. If this stream ever grows a filename rule of its own, this
// is what notices.
func TestCaptureSequenceContinuesAfterResume(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "mcp")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Simulate a crashed run that already captured 000..007.
	if err := os.WriteFile(filepath.Join(sub, "007-echo.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("seeding capture file: %v", err)
	}

	s, _ := newSession(t, Config{CaptureDir: dir})
	res, err := s.CallTool(context.Background(), "echo", json.RawMessage(`{"text":"resumed"}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.CaptureRef != "mcp/008-echo.json" {
		t.Errorf("CaptureRef = %q, want mcp/008-echo.json", res.CaptureRef)
	}
	if _, err := os.Stat(filepath.Join(sub, "008-echo.json")); err != nil {
		t.Errorf("expected sequence to continue at 008: %v", err)
	}
}

// TestCaptureSequenceContinuesPastThreeDigits pins the resume behaviour once
// a case has left four-digit capture files behind: a fixed-width parser skips
// 1000-echo.json, restarts numbering, and overwrites earlier captures.
func TestCaptureSequenceContinuesPastThreeDigits(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "mcp")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	seeded := []string{"000-echo.json", "999-echo.json", "1000-echo.json"}
	for _, name := range seeded {
		if err := os.WriteFile(filepath.Join(sub, name), []byte("{}\n"), 0o644); err != nil {
			t.Fatalf("seeding capture file %s: %v", name, err)
		}
	}

	s, _ := newSession(t, Config{CaptureDir: dir})
	res, err := s.CallTool(context.Background(), "echo", json.RawMessage(`{"text":"resumed"}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.CaptureRef != "mcp/1001-echo.json" {
		t.Errorf("CaptureRef = %q, want mcp/1001-echo.json", res.CaptureRef)
	}
	for _, name := range seeded {
		data, err := os.ReadFile(filepath.Join(sub, name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		if string(data) != "{}\n" {
			t.Errorf("%s was overwritten: sequence numbering collided", name)
		}
	}
}

// TestCaptureToolNameCannotEscapeTheCaptureDir pins the reason the tool name
// is sanitized: it is chosen by the server, not by us, so an unsanitized name
// would let a hostile or buggy server steer the capture write anywhere the
// process can write. The call itself fails — the fake server has no such tool
// — but a failed call is still captured, which is exactly the path that has to
// stay inside the capture dir.
func TestCaptureToolNameCannotEscapeTheCaptureDir(t *testing.T) {
	tests := []struct {
		name string
		tool string
		want string
	}{
		{"parent traversal", "../../../pwned", "case/captures/mcp/000-.._.._.._pwned.json"},
		{"path separator", "sub/dir", "case/captures/mcp/000-sub_dir.json"},
		{"absolute path", "/etc/passwd", "case/captures/mcp/000-_etc_passwd.json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "case", "captures")
			s, _ := newSession(t, Config{CaptureDir: dir})

			// Whether the server rejects the name or accepts it is not the
			// point; the capture write happens either way.
			_, _ = s.CallTool(context.Background(), tt.tool, json.RawMessage(`{"text":"x"}`))

			got := filesUnder(t, root)
			if len(got) != 1 || got[0] != tt.want {
				t.Errorf("files written = %v, want exactly [%s]", got, tt.want)
			}
		})
	}
}

// TestCaptureRefStaysInsideTheCaptureDir covers the other half: CaptureRef is
// what a Finding cites (spec 002 §3.4), and it is rendered from the same
// server-supplied tool name. A ref that pointed outside captures/ would send a
// reader — or the M4 grader — somewhere the capture is not.
func TestCaptureRefStaysInsideTheCaptureDir(t *testing.T) {
	dir := t.TempDir()
	s, _ := newSession(t, Config{CaptureDir: dir})
	res, err := s.CallTool(context.Background(), "echo", json.RawMessage(`{"text":"x"}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.CaptureRef != "mcp/000-echo.json" {
		t.Fatalf("CaptureRef = %q, want mcp/000-echo.json", res.CaptureRef)
	}
	// The ref resolves to the file that was actually written.
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(res.CaptureRef))); err != nil {
		t.Errorf("CaptureRef %q does not resolve under the capture dir: %v", res.CaptureRef, err)
	}
}

// filesUnder lists every regular file below root, relative to root and
// slash-separated, so a test can assert that nothing was written outside the
// directory it expected.
func filesUnder(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	sort.Strings(out)
	return out
}

func TestCaptureWriteFailureFailsCall(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; read-only dirs do not block writes")
	}
	s, dir := newSession(t, Config{})
	sub := filepath.Join(dir, "mcp")
	if err := os.Chmod(sub, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(sub, 0o755) })

	_, err := s.CallTool(context.Background(), "echo", json.RawMessage(`{"text":"x"}`))
	if err == nil {
		t.Fatal("call with unwritable capture dir returned nil error")
	}
	if !strings.Contains(err.Error(), "capture") {
		t.Errorf("err = %v, want it to name the capture write", err)
	}
}

func TestConnectValidatesConfig(t *testing.T) {
	ctx := context.Background()
	if _, err := Connect(ctx, Config{CaptureDir: t.TempDir()}); err == nil {
		t.Error("Connect without Command returned nil error")
	}
	if _, err := Connect(ctx, Config{Command: "/bin/true"}); err == nil {
		t.Error("Connect without CaptureDir returned nil error")
	}
}

func TestConnectFailsForMissingBinary(t *testing.T) {
	_, err := Connect(context.Background(), Config{
		Command:    filepath.Join(t.TempDir(), "no-such-binary"),
		CaptureDir: t.TempDir(),
	})
	if err == nil {
		t.Fatal("Connect to a missing binary returned nil error")
	}
}

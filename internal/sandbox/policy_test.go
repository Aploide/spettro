package sandbox_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"spettro/internal/sandbox"
)

func TestPolicyZeroValueDisabled(t *testing.T) {
	var p sandbox.Policy
	if p.Enabled() {
		t.Fatal("zero-value policy must be disabled")
	}
	if got := p.Summary(); got != "disabled" {
		t.Fatalf("Summary() = %q, want %q", got, "disabled")
	}
}

func TestResolvePolicyPrecedence(t *testing.T) {
	cases := []struct {
		name    string
		o       sandbox.Overrides
		m       sandbox.ManifestPolicy
		wantFS  sandbox.FSPolicy
		wantNet sandbox.NetPolicy
	}{
		{name: "all unset is disabled", wantFS: sandbox.FSOff, wantNet: sandbox.NetAll},
		{name: "manifest mode applies", m: sandbox.ManifestPolicy{Mode: "read-only"}, wantFS: sandbox.FSReadOnly, wantNet: sandbox.NetAll},
		{name: "manifest full-access means off", m: sandbox.ManifestPolicy{Mode: "full-access"}, wantFS: sandbox.FSOff, wantNet: sandbox.NetAll},
		{name: "cli off beats manifest", o: sandbox.Overrides{Mode: "off"}, m: sandbox.ManifestPolicy{Mode: "read-only"}, wantFS: sandbox.FSOff, wantNet: sandbox.NetAll},
		{name: "cli mode beats manifest", o: sandbox.Overrides{Mode: "read-only"}, m: sandbox.ManifestPolicy{Mode: "workspace-write"}, wantFS: sandbox.FSReadOnly, wantNet: sandbox.NetAll},
		{name: "cli net beats manifest net", o: sandbox.Overrides{Net: "ports:443"}, m: sandbox.ManifestPolicy{Net: "none"}, wantFS: sandbox.FSOff, wantNet: sandbox.NetPorts},
		{name: "manifest net applies when cli unset", m: sandbox.ManifestPolicy{Net: "localhost"}, wantFS: sandbox.FSOff, wantNet: sandbox.NetLocalhost},
		{name: "net-only policy is enabled", o: sandbox.Overrides{Net: "none"}, wantFS: sandbox.FSOff, wantNet: sandbox.NetNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := sandbox.ResolvePolicy(tc.o, tc.m)
			if err != nil {
				t.Fatalf("ResolvePolicy: %v", err)
			}
			if p.FS != tc.wantFS || p.Net != tc.wantNet {
				t.Fatalf("got fs=%q net=%q, want fs=%q net=%q", p.FS, p.Net, tc.wantFS, tc.wantNet)
			}
			wantEnabled := tc.wantFS != sandbox.FSOff || tc.wantNet != sandbox.NetAll
			if p.Enabled() != wantEnabled {
				t.Fatalf("Enabled() = %v, want %v", p.Enabled(), wantEnabled)
			}
		})
	}
}

func TestResolvePolicyInvalidInputs(t *testing.T) {
	if _, err := sandbox.ResolvePolicy(sandbox.Overrides{Mode: "bogus"}, sandbox.ManifestPolicy{}); err == nil {
		t.Fatal("invalid cli mode must error")
	}
	if _, err := sandbox.ResolvePolicy(sandbox.Overrides{}, sandbox.ManifestPolicy{Mode: "bogus"}); err == nil {
		t.Fatal("invalid manifest mode must error")
	}
	if _, err := sandbox.ResolvePolicy(sandbox.Overrides{Net: "bogus"}, sandbox.ManifestPolicy{}); err == nil {
		t.Fatal("invalid net spec must error")
	}
}

func TestResolvePolicyAllowDirs(t *testing.T) {
	dir := t.TempDir()
	p, err := sandbox.ResolvePolicy(
		sandbox.Overrides{AllowDirs: []string{dir}},
		sandbox.ManifestPolicy{AllowDirs: []string{dir}}, // duplicate must collapse
	)
	if err != nil {
		t.Fatalf("ResolvePolicy: %v", err)
	}
	abs, _ := filepath.Abs(dir)
	if !reflect.DeepEqual(p.ExtraWritable, []string{abs}) {
		t.Fatalf("ExtraWritable = %v, want [%s]", p.ExtraWritable, abs)
	}

	if _, err := sandbox.ResolvePolicy(sandbox.Overrides{AllowDirs: []string{filepath.Join(dir, "missing")}}, sandbox.ManifestPolicy{}); err == nil {
		t.Fatal("nonexistent allow dir must error")
	}

	file := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := sandbox.ResolvePolicy(sandbox.Overrides{AllowDirs: []string{file}}, sandbox.ManifestPolicy{}); err == nil {
		t.Fatal("non-directory allow dir must error")
	}
}

func TestParseMode(t *testing.T) {
	for in, want := range map[string]sandbox.FSPolicy{
		"off":             sandbox.FSOff,
		"full-access":     sandbox.FSOff,
		"read-only":       sandbox.FSReadOnly,
		"workspace-write": sandbox.FSWorkspaceWrite,
		" Read-Only ":     sandbox.FSReadOnly,
	} {
		got, err := sandbox.ParseMode(in)
		if err != nil || got != want {
			t.Fatalf("ParseMode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "bogus", "readonly"} {
		if _, err := sandbox.ParseMode(in); err == nil {
			t.Fatalf("ParseMode(%q) must error", in)
		}
	}
}

func TestParseNetSpec(t *testing.T) {
	n, ports, err := sandbox.ParseNetSpec("ports:8080,443,443")
	if err != nil || n != sandbox.NetPorts {
		t.Fatalf("ParseNetSpec(ports:...) = %q, %v", n, err)
	}
	if !reflect.DeepEqual(ports, []uint16{443, 8080}) {
		t.Fatalf("ports = %v, want deduped sorted [443 8080]", ports)
	}
	for in, want := range map[string]sandbox.NetPolicy{
		"all":       sandbox.NetAll,
		"localhost": sandbox.NetLocalhost,
		"none":      sandbox.NetNone,
	} {
		n, ports, err := sandbox.ParseNetSpec(in)
		if err != nil || n != want || ports != nil {
			t.Fatalf("ParseNetSpec(%q) = %q, %v, %v; want %q", in, n, ports, err, want)
		}
	}
	for _, in := range []string{"", "bogus", "ports:", "ports:0", "ports:70000", "ports:abc"} {
		if _, _, err := sandbox.ParseNetSpec(in); err == nil {
			t.Fatalf("ParseNetSpec(%q) must error", in)
		}
	}
}

func TestPolicyWritablePath(t *testing.T) {
	ws := filepath.Clean("/work/repo")
	tmp := filepath.Clean("/tmp")
	extra := filepath.Clean("/data")
	temps := []string{tmp}

	inside := filepath.Join(ws, "src/main.go")
	inTmp := filepath.Join(tmp, "scratch")
	inExtra := filepath.Join(extra, "out.bin")
	outside := filepath.Clean("/etc/passwd")

	cases := []struct {
		name string
		p    sandbox.Policy
		path string
		want bool
	}{
		{"off allows anything", sandbox.Policy{}, outside, true},
		{"workspace-write allows workspace", sandbox.Policy{FS: sandbox.FSWorkspaceWrite}, inside, true},
		{"workspace-write allows temp", sandbox.Policy{FS: sandbox.FSWorkspaceWrite}, inTmp, true},
		{"workspace-write denies outside", sandbox.Policy{FS: sandbox.FSWorkspaceWrite}, outside, false},
		{"read-only denies workspace", sandbox.Policy{FS: sandbox.FSReadOnly}, inside, false},
		{"read-only allows temp", sandbox.Policy{FS: sandbox.FSReadOnly}, inTmp, true},
		{"read-only allows extra writable", sandbox.Policy{FS: sandbox.FSReadOnly, ExtraWritable: []string{extra}}, inExtra, true},
		{"read-only denies workspace even with extra", sandbox.Policy{FS: sandbox.FSReadOnly, ExtraWritable: []string{extra}}, inside, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.WritablePath(tc.path, ws, temps); got != tc.want {
				t.Fatalf("WritablePath(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

func TestPolicySummaryAndShort(t *testing.T) {
	p := sandbox.Policy{FS: sandbox.FSReadOnly, Net: sandbox.NetPorts, AllowedPorts: []uint16{443, 8080}, ExtraWritable: []string{"/data"}}
	sum := p.Summary()
	for _, want := range []string{"fs=read-only", "net=ports:443,8080", "/data"} {
		if !strings.Contains(sum, want) {
			t.Fatalf("Summary() = %q, missing %q", sum, want)
		}
	}
	if got := p.Short(); got != "ro+net:443,8080" {
		t.Fatalf("Short() = %q", got)
	}
	if got := (sandbox.Policy{FS: sandbox.FSWorkspaceWrite, Net: sandbox.NetNone}).Short(); got != "ws+net:none" {
		t.Fatalf("Short() = %q", got)
	}
}

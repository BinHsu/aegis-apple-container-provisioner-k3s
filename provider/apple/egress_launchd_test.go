// SPDX-License-Identifier: MIT

package apple

import (
	"encoding/xml"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestEgressLabel(t *testing.T) {
	for name, ok := range map[string]bool{
		"tw-poc": true, "a": true, "c1.lab_2": true,
		"": false, "-x": false, "a/b": false, "a b": false, "../x": false, "x;rm": false,
	} {
		got, err := egressLabel(name)
		if (err == nil) != ok {
			t.Errorf("egressLabel(%q) err=%v, want ok=%v", name, err, ok)
		}

		if ok && got != egressLabelPrefix+name {
			t.Errorf("egressLabel(%q) = %q", name, got)
		}
	}
}

func TestParseLaunchdPID(t *testing.T) {
	running := "gui/501/io.github.binhsu.k3ac.egress.tw-poc = {\n\tactive count = 1\n\tpath = /x/egress.plist\n" +
		"\tstate = running\n\n\tprogram = /x/ac-egress\n\tpid = 16966\n\tlast exit code = (never exited)\n" +
		"\tendpoints = {\n\t\tpid = 7\n\t}\n}\n"
	if got := parseLaunchdPID(running); got != 16966 {
		t.Errorf("running job: got %d", got)
	}

	exited := "gui/501/x = {\n\tstate = not running\n\tendpoints = {\n\t\tpid = 7\n\t}\n\tlast exit code = 1\n}\n"
	if got := parseLaunchdPID(exited); got != 0 {
		t.Errorf("exited job: nested pid must be ignored, got %d", got)
	}

	for _, bad := range []string{"", "\tpid = abc\n", "\tpid = 1\n", "\tpid = 0\n"} {
		if got := parseLaunchdPID(bad); got != 0 {
			t.Errorf("parseLaunchdPID(%q) = %d", bad, got)
		}
	}
}

func TestRelayCommandFor(t *testing.T) {
	gw := netip.MustParseAddr("192.168.200.1")

	for line, want := range map[string]bool{
		"/Users/x/_out/ac-egress -listen 192.168.200.1\n": true,
		"/Users/x/_out/ac-egress -listen 192.168.200.10":  false, // prefix of another address
		"/Users/x/_out/ac-egress -listen 192.168.64.1":    false, // other bridge: restart, do not reuse
		"/bin/zsh -c ac-egress -listen 192.168.200.1":     false,
		"/Users/x/_out/ac-egress -listen":                 false,
		"":                                                false,
	} {
		if got := relayCommandFor(line, gw); got != want {
			t.Errorf("relayCommandFor(%q) = %v, want %v", line, got, want)
		}
	}
}

func TestRelayPlist(t *testing.T) {
	gw := netip.MustParseAddr("192.168.200.1")
	out := relayPlist("io.github.binhsu.k3ac.egress.tw-poc", "/a b/<x>&/ac-egress", gw, "/s/egress.log")

	// Well-formed XML, values escaped, arguments in order, transient (no KeepAlive restarts).
	dec := xml.NewDecoder(strings.NewReader(out))
	dec.Strict = true
	dec.Entity = xml.HTMLEntity

	var strs []string

	for {
		tok, err := dec.Token()
		if err != nil {
			if err.Error() != "EOF" {
				t.Fatalf("plist is not well-formed XML: %v\n%s", err, out)
			}

			break
		}

		if cd, ok := tok.(xml.CharData); ok && strings.TrimSpace(string(cd)) != "" {
			strs = append(strs, string(cd))
		}
	}

	want := []string{"Label", "io.github.binhsu.k3ac.egress.tw-poc", "ProgramArguments",
		"/a b/<x>&/ac-egress", "-listen", "192.168.200.1",
		"StandardOutPath", "/s/egress.log", "StandardErrorPath", "/s/egress.log", "RunAtLoad", "KeepAlive"}
	if strings.Join(strs, "|") != strings.Join(want, "|") {
		t.Errorf("plist values\n got %q\nwant %q", strs, want)
	}

	if !strings.Contains(out, "<key>KeepAlive</key><false/>") || !strings.Contains(out, "<key>RunAtLoad</key><true/>") {
		t.Errorf("plist must be RunAtLoad true, KeepAlive false:\n%s", out)
	}

	// plutil is the authority on plist syntax where it exists (macOS).
	if plutil, err := exec.LookPath("plutil"); err == nil {
		p := filepath.Join(t.TempDir(), "egress.plist")
		if err := writeFile(p, out); err != nil {
			t.Fatal(err)
		}

		if msg, err := exec.Command(plutil, "-lint", p).CombinedOutput(); err != nil {
			t.Errorf("plutil -lint: %v: %s", err, msg)
		}
	} else {
		t.Log("plutil not found: launchd syntax check skipped (XML well-formedness still checked)")
	}
}

func writeFile(path, s string) error { return os.WriteFile(path, []byte(s), 0o600) }

func TestInstallRelayBinary(t *testing.T) {
	src := filepath.Join(t.TempDir(), "ac-egress")
	run := t.TempDir()

	if err := writeFile(src, "v1"); err != nil {
		t.Fatal(err)
	}

	dst, err := installRelayBinary(src, run)
	if err != nil || dst != filepath.Join(run, EgressBinary) {
		t.Fatalf("first install: %q, %v", dst, err)
	}

	st1, _ := os.Stat(dst)
	if st1.Mode().Perm() != 0o700 {
		t.Errorf("mode %v, want 0700", st1.Mode().Perm())
	}

	// Identical source: the copy (inode) is kept, so a running relay is not disturbed.
	if _, err := installRelayBinary(src, run); err != nil {
		t.Fatal(err)
	}

	if st2, _ := os.Stat(dst); !os.SameFile(st1, st2) {
		t.Error("identical binary was rewritten")
	}

	// Changed source: replaced by rename (new inode), content updated, no temp files left.
	if err := writeFile(src, "v2"); err != nil {
		t.Fatal(err)
	}

	if _, err := installRelayBinary(src, run); err != nil {
		t.Fatal(err)
	}

	st3, _ := os.Stat(dst)
	if b, _ := os.ReadFile(dst); string(b) != "v2" || os.SameFile(st1, st3) {
		t.Errorf("changed binary not replaced by rename: %q", b)
	}

	if ents, _ := os.ReadDir(run); len(ents) != 1 {
		t.Errorf("run dir has %d entries, want only the binary", len(ents))
	}
}

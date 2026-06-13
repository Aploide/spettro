package sandbox

import "testing"

var (
	goldenTempDirs  = []string{"/var/tmp-test", "/private/tmp", "/private/var/folders", "/dev"}
	goldenHomeRoots = []string{"/Users", "/home"}
)

func golden(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("profile mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestSeatbeltProfileWorkspaceWrite(t *testing.T) {
	got := seatbeltProfile(Policy{FS: FSWorkspaceWrite}, "/ws", goldenTempDirs, goldenHomeRoots)
	golden(t, got, `(version 1)
(allow default)
(deny file-write*)
(allow file-write* (subpath "/ws"))
(allow file-write* (subpath "/var/tmp-test"))
(allow file-write* (subpath "/private/tmp"))
(allow file-write* (subpath "/private/var/folders"))
(allow file-write* (subpath "/dev"))
(deny file-read* (subpath "/Users"))
(deny file-read* (subpath "/home"))
(allow file-read* (subpath "/ws"))
`)
}

func TestSeatbeltProfileReadOnlyNetNone(t *testing.T) {
	got := seatbeltProfile(Policy{FS: FSReadOnly, Net: NetNone}, "/ws", goldenTempDirs, goldenHomeRoots)
	golden(t, got, `(version 1)
(allow default)
(deny file-write*)
(allow file-write* (subpath "/var/tmp-test"))
(allow file-write* (subpath "/private/tmp"))
(allow file-write* (subpath "/private/var/folders"))
(allow file-write* (subpath "/dev"))
(deny file-read* (subpath "/Users"))
(deny file-read* (subpath "/home"))
(allow file-read* (subpath "/ws"))
(deny network*)
`)
}

func TestSeatbeltProfileExtraWritableAndReadable(t *testing.T) {
	got := seatbeltProfile(Policy{
		FS:            FSReadOnly,
		ExtraWritable: []string{"/data"},
		ExtraReadable: []string{"/cache/go"},
	}, "/ws", goldenTempDirs, goldenHomeRoots)
	golden(t, got, `(version 1)
(allow default)
(deny file-write*)
(allow file-write* (subpath "/var/tmp-test"))
(allow file-write* (subpath "/private/tmp"))
(allow file-write* (subpath "/private/var/folders"))
(allow file-write* (subpath "/dev"))
(allow file-write* (subpath "/data"))
(deny file-read* (subpath "/Users"))
(deny file-read* (subpath "/home"))
(allow file-read* (subpath "/ws"))
(allow file-read* (subpath "/cache/go"))
(allow file-read* (subpath "/data"))
`)
}

func TestBrokerSeatbeltProfile(t *testing.T) {
	// The parent broker write-confines only: reads and network stay open.
	got := brokerSeatbeltProfile([]string{"/home/u/.spettro", "/work/repo", "/tmp"})
	golden(t, got, `(version 1)
(allow default)
(deny file-write*)
(allow file-write* (subpath "/home/u/.spettro"))
(allow file-write* (subpath "/work/repo"))
(allow file-write* (subpath "/tmp"))
`)
}

func TestSeatbeltProfileNetOnlyLeavesReadsOpen(t *testing.T) {
	// Network-only confinement must not block reads (FS is unconfined).
	got := seatbeltProfile(Policy{Net: NetLocalhost}, "/ws", goldenTempDirs, goldenHomeRoots)
	golden(t, got, `(version 1)
(allow default)
(deny network*)
(allow network* (local ip "localhost:*"))
(allow network* (remote ip "localhost:*"))
(allow network-outbound (literal "/private/var/run/mDNSResponder"))
`)
}

func TestSeatbeltProfileNetPorts(t *testing.T) {
	got := seatbeltProfile(Policy{Net: NetPorts, AllowedPorts: []uint16{443, 8080}}, "/ws", goldenTempDirs, goldenHomeRoots)
	golden(t, got, `(version 1)
(allow default)
(deny network*)
(allow network-outbound (remote ip "*:443"))
(allow network-bind (local ip "*:443"))
(allow network-inbound (local ip "*:443"))
(allow network-outbound (remote ip "*:8080"))
(allow network-bind (local ip "*:8080"))
(allow network-inbound (local ip "*:8080"))
(allow network-outbound (literal "/private/var/run/mDNSResponder"))
`)
}

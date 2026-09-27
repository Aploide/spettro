package main

import "testing"

func TestPrintVersionIfRequested(t *testing.T) {
	for _, args := range [][]string{{"--version"}, {"-v"}, {"-version"}, {"version"}} {
		if !printVersionIfRequested(args) {
			t.Errorf("%v: version not printed", args)
		}
	}
	for _, args := range [][]string{nil, {"--help"}, {"version", "extra"}, {"--acp"}, {"clean"}} {
		if printVersionIfRequested(args) {
			t.Errorf("%v: treated as a version request", args)
		}
	}
}

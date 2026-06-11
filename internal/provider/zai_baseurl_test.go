package provider

import (
	"net/url"
	"testing"
)

// Guards against the api.zai.ai typo, which is an NXDOMAIN and caused
// "dial tcp: lookup api.zai.ai: no such host" for the zai provider.
func TestZAIBaseURLHost(t *testing.T) {
	raw, ok := knownBaseURLs["zai"]
	if !ok {
		t.Fatal("zai base url missing from knownBaseURLs")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse zai base url %q: %v", raw, err)
	}
	if u.Host != "api.z.ai" {
		t.Fatalf("zai base url host = %q, want api.z.ai", u.Host)
	}
}

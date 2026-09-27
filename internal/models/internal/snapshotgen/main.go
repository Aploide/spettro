// Command snapshotgen downloads the Spettro provider catalog and writes the
// gzip-compressed snapshot that internal/models embeds as its offline
// fallback. It runs from `go generate ./internal/models`, which the release
// workflow does before building so every release ships a current snapshot.
//
// It is deliberately standalone (it does not import internal/models, whose
// build needs the snapshot file to exist), so it can recreate a missing
// snapshot.
package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

func main() {
	url := flag.String("url", "https://catalog.spettro.app/providers.min.json", "catalog URL")
	out := flag.String("o", "catalog_snapshot.json.gz", "output file")
	flag.Parse()
	if err := run(*url, *out); err != nil {
		fmt.Fprintf(os.Stderr, "snapshotgen: %v\n", err)
		os.Exit(1)
	}
}

func run(url, out string) error {
	body, err := download(url)
	if err != nil {
		return err
	}
	if err := validate(body); err != nil {
		return err
	}
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return err
	}
	if _, err := zw.Write(body); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return os.WriteFile(out, buf.Bytes(), 0o644)
}

func download(url string) ([]byte, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// validate refuses to write a snapshot the CLI could not use.
func validate(body []byte) error {
	var doc struct {
		Updated   string                     `json:"updated"`
		Providers map[string]json.RawMessage `json:"providers"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return fmt.Errorf("catalog parse: %w", err)
	}
	if len(doc.Providers) == 0 || doc.Updated == "" {
		return fmt.Errorf("catalog has no providers or no updated date")
	}
	return nil
}

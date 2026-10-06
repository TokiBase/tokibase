package fshttp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"path"

	"github.com/tokibase/tokibase/tools/filesystem"
)

// NewFileFromURL creates a new File from the provided url by
// downloading the resource and load it as BytesReader.
//
// Example
//
//	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
//	defer cancel()
//
//	file, err := filesystem.NewFileFromURL(ctx, "https://example.com/image.png")
func NewFileFromURL(ctx context.Context, url string) (*filesystem.File, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode > 399 {
		return nil, fmt.Errorf("failed to download url %s (%d)", url, res.StatusCode)
	}

	var buf bytes.Buffer

	if _, err = io.Copy(&buf, res.Body); err != nil {
		return nil, err
	}

	return filesystem.NewFileFromBytes(buf.Bytes(), path.Base(url))
}

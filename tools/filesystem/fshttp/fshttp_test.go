package fshttp_test

import (
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/tokibase/tokibase/tools/filesystem"
	"github.com/tokibase/tokibase/tools/filesystem/fshttp"
)

func TestFilesystemServe(t *testing.T) {
	dir := createTestDir(t)
	defer os.RemoveAll(dir)

	fsys, err := filesystem.NewLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer fsys.Close()

	hookCalls := bindHooks(fsys)

	csp := "default-src 'none'; media-src 'self'; style-src 'unsafe-inline'; sandbox"
	cacheControl := "max-age=2592000, stale-while-revalidate=86400"

	scenarios := []struct {
		path          string
		name          string
		query         map[string]string
		headers       map[string]string
		expectError   bool
		expectHeaders map[string]string
	}{
		{
			// missing
			"missing.txt",
			"test_name.txt",
			nil,
			nil,
			true,
			nil,
		},
		{
			// existing regular file
			"test/sub1.txt",
			"test_name.txt",
			nil,
			nil,
			false,
			map[string]string{
				"Content-Disposition":     `attachment; filename="test_name.txt"`,
				"Content-Type":            "application/octet-stream",
				"Content-Length":          "4",
				"Content-Security-Policy": csp,
				"Cache-Control":           cacheControl,
			},
		},
		{
			// png inline
			"image.png",
			"test_name.png",
			nil,
			nil,
			false,
			map[string]string{
				"Content-Disposition":     `inline; filename="test_name.png"`,
				"Content-Type":            "image/png",
				"Content-Length":          "77",
				"Content-Security-Policy": csp,
				"Cache-Control":           cacheControl,
			},
		},
		{
			// png with forced attachment
			"image.png",
			"test_name_download.png",
			map[string]string{"download": "1"},
			nil,
			false,
			map[string]string{
				"Content-Disposition":     `attachment; filename="test_name_download.png"`,
				"Content-Type":            "image/png",
				"Content-Length":          "77",
				"Content-Security-Policy": csp,
				"Cache-Control":           cacheControl,
			},
		},
		{
			// svg exception
			"image.svg",
			"test_name.abc",
			nil,
			nil,
			false,
			map[string]string{
				"Content-Disposition":     `attachment; filename="test_name.abc"`,
				"Content-Type":            "image/svg+xml",
				"Content-Length":          "0",
				"Content-Security-Policy": csp,
				"Cache-Control":           cacheControl,
			},
		},
		{
			// css exception
			"style.css",
			"test_name",
			nil,
			nil,
			false,
			map[string]string{
				"Content-Disposition":     `attachment; filename="test_name"`,
				"Content-Type":            "text/css",
				"Content-Length":          "0",
				"Content-Security-Policy": csp,
				"Cache-Control":           cacheControl,
			},
		},
		{
			// js exception
			"main.js",
			"test_name.abc",
			nil,
			nil,
			false,
			map[string]string{
				"Content-Disposition":     `attachment; filename="test_name.abc"`,
				"Content-Type":            "text/javascript",
				"Content-Length":          "0",
				"Content-Security-Policy": csp,
				"Cache-Control":           cacheControl,
			},
		},
		{
			// mjs exception
			"main.mjs",
			"test_name.abc",
			nil,
			nil,
			false,
			map[string]string{
				"Content-Disposition":     `attachment; filename="test_name.abc"`,
				"Content-Type":            "text/javascript",
				"Content-Length":          "0",
				"Content-Security-Policy": csp,
				"Cache-Control":           cacheControl,
			},
		},
		{
			// xlsx exception
			"dummy.xlsx",
			"test_name.abc",
			nil,
			nil,
			false,
			map[string]string{
				"Content-Disposition":     `attachment; filename="test_name.abc"`,
				"Content-Type":            "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
				"Content-Length":          "0",
				"Content-Security-Policy": csp,
				"Cache-Control":           cacheControl,
			},
		},
		{
			// docx exception
			"dummy.docx",
			"test_name.abc",
			nil,
			nil,
			false,
			map[string]string{
				"Content-Disposition":     `attachment; filename="test_name.abc"`,
				"Content-Type":            "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
				"Content-Length":          "0",
				"Content-Security-Policy": csp,
				"Cache-Control":           cacheControl,
			},
		},
		{
			// pptx exception
			"dummy.pptx",
			"test_name.abc",
			nil,
			nil,
			false,
			map[string]string{
				"Content-Disposition":     `attachment; filename="test_name.abc"`,
				"Content-Type":            "application/vnd.openxmlformats-officedocument.presentationml.presentation",
				"Content-Length":          "0",
				"Content-Security-Policy": csp,
				"Cache-Control":           cacheControl,
			},
		},
		{
			// custom header
			"test/sub2.txt",
			"test_name.txt",
			nil,
			map[string]string{
				"Content-Disposition":     "1",
				"Content-Type":            "2",
				"Content-Length":          "1",
				"Content-Security-Policy": "4",
				"Cache-Control":           "5",
				"X-Custom":                "6",
			},
			false,
			map[string]string{
				"Content-Disposition":     "1",
				"Content-Type":            "2",
				"Content-Length":          "4", // overwriten by http.ServeContent
				"Content-Security-Policy": "4",
				"Cache-Control":           "5",
				"X-Custom":                "6",
			},
		},
	}

	for _, s := range scenarios {
		t.Run(s.path, func(t *testing.T) {
			res := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/", nil)

			query := req.URL.Query()
			for k, v := range s.query {
				query.Set(k, v)
			}
			req.URL.RawQuery = query.Encode()

			for k, v := range s.headers {
				res.Header().Set(k, v)
			}

			err := fshttp.Serve(fsys, res, req, s.path, s.name)
			hasErr := err != nil

			if hasErr != s.expectError {
				t.Fatalf("Expected hasError %v, got %v (%v)", s.expectError, hasErr, err)
			}

			if s.expectError {
				return
			}

			result := res.Result()
			defer result.Body.Close()

			for hName, hValue := range s.expectHeaders {
				v := result.Header.Get(hName)
				if v != hValue {
					t.Errorf("Expected value %q for header %q, got %q", hValue, hName, v)
				}
			}
		})
	}

	checkHooks(t, hookCalls, map[string]int{"OnDelete": 0, "OnNewWriter": 0})
}

func TestFilesystemServeSingleRange(t *testing.T) {
	dir := createTestDir(t)
	defer os.RemoveAll(dir)

	fsys, err := filesystem.NewLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer fsys.Close()

	hookCalls := bindHooks(fsys)

	res := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Add("Range", "bytes=0-20")

	if err := fshttp.Serve(fsys, res, req, "image.png", "image.png"); err != nil {
		t.Fatal(err)
	}

	result := res.Result()

	if result.StatusCode != http.StatusPartialContent {
		t.Fatalf("Expected StatusCode %d, got %d", http.StatusPartialContent, result.StatusCode)
	}

	expectedRange := "bytes 0-20/77"
	if cr := result.Header.Get("Content-Range"); cr != expectedRange {
		t.Fatalf("Expected Content-Range %q, got %q", expectedRange, cr)
	}

	if l := result.Header.Get("Content-Length"); l != "21" {
		t.Fatalf("Expected Content-Length %v, got %v", 21, l)
	}

	checkHooks(t, hookCalls, map[string]int{"OnDelete": 0, "OnNewWriter": 0})
}

func TestFilesystemServeMultiRange(t *testing.T) {
	dir := createTestDir(t)
	defer os.RemoveAll(dir)

	fsys, err := filesystem.NewLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer fsys.Close()

	hookCalls := bindHooks(fsys)

	res := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Add("Range", "bytes=0-20, 25-30")

	if err := fshttp.Serve(fsys, res, req, "image.png", "image.png"); err != nil {
		t.Fatal(err)
	}

	result := res.Result()

	if result.StatusCode != http.StatusPartialContent {
		t.Fatalf("Expected StatusCode %d, got %d", http.StatusPartialContent, result.StatusCode)
	}

	if ct := result.Header.Get("Content-Type"); !strings.HasPrefix(ct, "multipart/byteranges; boundary=") {
		t.Fatalf("Expected Content-Type to be multipart/byteranges, got %v", ct)
	}

	checkHooks(t, hookCalls, map[string]int{"OnDelete": 0, "OnNewWriter": 0})
}

func TestNewFileFromURLTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/error" {
			w.WriteHeader(http.StatusInternalServerError)
		}

		fmt.Fprintf(w, "test")
	}))
	defer srv.Close()

	// cancelled context
	{
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		f, err := fshttp.NewFileFromURL(ctx, srv.URL+"/cancel")
		if err == nil {
			t.Fatal("[ctx_cancel] Expected error, got nil")
		}
		if f != nil {
			t.Fatalf("[ctx_cancel] Expected file to be nil, got %v", f)
		}
	}

	// error response
	{
		f, err := fshttp.NewFileFromURL(context.Background(), srv.URL+"/error")
		if err == nil {
			t.Fatal("[error_status] Expected error, got nil")
		}
		if f != nil {
			t.Fatalf("[error_status] Expected file to be nil, got %v", f)
		}
	}

	// valid response
	{
		originalName := "image_!@ special"
		normalizedNamePattern := regexp.QuoteMeta("image_special_") + `\w{10}` + regexp.QuoteMeta(".txt")

		f, err := fshttp.NewFileFromURL(context.Background(), srv.URL+"/"+originalName)
		if err != nil {
			t.Fatalf("[valid] Unexpected error %v", err)
		}
		if f == nil {
			t.Fatal("[valid] Expected non-nil file")
		}

		// check the created file fields
		if f.OriginalName != originalName {
			t.Fatalf("Expected OriginalName %q, got %q", originalName, f.OriginalName)
		}
		if match, err := regexp.Match(normalizedNamePattern, []byte(f.Name)); !match {
			t.Fatalf("Expected Name to match %v, got %q (%v)", normalizedNamePattern, f.Name, err)
		}
		if f.Size != 4 {
			t.Fatalf("Expected Size %v, got %v", 4, f.Size)
		}
		if _, ok := f.Reader.(*filesystem.BytesReader); !ok {
			t.Fatalf("Expected Reader to be BytesReader, got %v", f.Reader)
		}
	}
}

func createTestDir(t *testing.T) string {
	dir, err := os.MkdirTemp(os.TempDir(), "pb_test")
	if err != nil {
		t.Fatal(err)
	}

	err = os.MkdirAll(filepath.Join(dir, "empty"), os.ModePerm)
	if err != nil {
		t.Fatal(err)
	}

	err = os.MkdirAll(filepath.Join(dir, "test"), os.ModePerm)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(dir, "test/sub1.txt"), []byte("sub1"), 0644)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(dir, "test/sub2.txt"), []byte("sub2"), 0644)
	if err != nil {
		t.Fatal(err)
	}

	// png
	{
		file, err := os.OpenFile(filepath.Join(dir, "image.png"), os.O_WRONLY|os.O_CREATE, 0644)
		if err != nil {
			t.Fatal(err)
		}
		imgRect := image.Rect(0, 0, 1, 1) // tiny 1x1 png
		_ = png.Encode(file, imgRect)
		file.Close()
		err = os.WriteFile(filepath.Join(dir, "image.png.attrs"), []byte(`{"user.cache_control":"","user.content_disposition":"","user.content_encoding":"","user.content_language":"","user.content_type":"image/png","user.metadata":null}`), 0644)
		if err != nil {
			t.Fatal(err)
		}
	}

	// jpg
	{
		file, err := os.OpenFile(filepath.Join(dir, "image.jpg"), os.O_WRONLY|os.O_CREATE, 0644)
		if err != nil {
			t.Fatal(err)
		}
		imgRect := image.Rect(0, 0, 1, 1) // tiny 1x1 jpg
		_ = jpeg.Encode(file, imgRect, nil)
		file.Close()
		err = os.WriteFile(filepath.Join(dir, "image.jpg.attrs"), []byte(`{"user.cache_control":"","user.content_disposition":"","user.content_encoding":"","user.content_language":"","user.content_type":"image/jpeg","user.metadata":null}`), 0644)
		if err != nil {
			t.Fatal(err)
		}
	}

	// svg
	{
		file, err := os.OpenFile(filepath.Join(dir, "image.svg"), os.O_WRONLY|os.O_CREATE, 0644)
		if err != nil {
			t.Fatal(err)
		}
		file.Close()
	}

	// webp
	{
		err := os.WriteFile(filepath.Join(dir, "image.webp"), []byte{
			82, 73, 70, 70, 36, 0, 0, 0, 87, 69, 66, 80, 86, 80, 56, 32,
			24, 0, 0, 0, 48, 1, 0, 157, 1, 42, 1, 0, 1, 0, 2, 0, 52, 37,
			164, 0, 3, 112, 0, 254, 251, 253, 80, 0,
		}, 0644)
		if err != nil {
			t.Fatal(err)
		}
	}

	// invalid/special characters
	{
		file, err := os.OpenFile(filepath.Join(dir, "image_!@ special"), os.O_WRONLY|os.O_CREATE, 0644)
		if err != nil {
			t.Fatal(err)
		}
		imgRect := image.Rect(0, 0, 1, 1) // tiny 1x1 png
		_ = png.Encode(file, imgRect)
		file.Close()
	}

	// no extension
	{
		fullPath := filepath.Join(dir, "image_noext")
		file, err := os.OpenFile(fullPath, os.O_WRONLY|os.O_CREATE, 0644)
		if err != nil {
			t.Fatal(err)
		}
		imgRect := image.Rect(0, 0, 1, 1) // tiny 1x1 jpg
		_ = jpeg.Encode(file, imgRect, nil)
		file.Close()
		err = os.WriteFile(fullPath+".attrs", []byte(`{"user.cache_control":"","user.content_disposition":"","user.content_encoding":"","user.content_language":"","user.content_type":"image/jpeg","user.metadata":null}`), 0644)
		if err != nil {
			t.Fatal(err)
		}
	}

	// css
	{
		file, err := os.OpenFile(filepath.Join(dir, "style.css"), os.O_WRONLY|os.O_CREATE, 0644)
		if err != nil {
			t.Fatal(err)
		}
		file.Close()
	}

	// js
	{
		file, err := os.OpenFile(filepath.Join(dir, "main.js"), os.O_WRONLY|os.O_CREATE, 0644)
		if err != nil {
			t.Fatal(err)
		}
		file.Close()
	}

	// mjs
	{
		file, err := os.OpenFile(filepath.Join(dir, "main.mjs"), os.O_WRONLY|os.O_CREATE, 0644)
		if err != nil {
			t.Fatal(err)
		}
		file.Close()
	}

	// "docx" (we are interested only in the extension)
	{
		file, err := os.OpenFile(filepath.Join(dir, "dummy.docx"), os.O_WRONLY|os.O_CREATE, 0644)
		if err != nil {
			t.Fatal(err)
		}
		file.Close()
	}

	// "xlsx" (we are interested only in the extension)
	{
		file, err := os.OpenFile(filepath.Join(dir, "dummy.xlsx"), os.O_WRONLY|os.O_CREATE, 0644)
		if err != nil {
			t.Fatal(err)
		}
		file.Close()
	}

	// "pptx" (we are interested only in the extension)
	{
		file, err := os.OpenFile(filepath.Join(dir, "dummy.pptx"), os.O_WRONLY|os.O_CREATE, 0644)
		if err != nil {
			t.Fatal(err)
		}
		file.Close()
	}

	return dir
}

func bindHooks(fsys *filesystem.System) map[string]int {
	hookCalls := map[string]int{}

	fsys.OnDelete().BindFunc(func(e *filesystem.DeleteEvent) error {
		hookCalls["OnDelete"]++
		return e.Next()
	})

	fsys.OnNewWriter().BindFunc(func(e *filesystem.NewWriterEvent) error {
		hookCalls["OnNewWriter"]++
		return e.Next()
	})

	return hookCalls
}

func checkHooks(t *testing.T, hookCalls, expectations map[string]int) {
	for event, expected := range expectations {
		got, _ := hookCalls[event]
		if got != expected {
			t.Fatalf("Expected event %q to be called %d, got %d", event, expected, got)
		}
	}
}

// Package fshttp contains the net/http dependent helpers of the filesystem
// package (HTTP serving, remote downloads and the S3 driver client), so that
// the filesystem package itself stays free from net/http.
package fshttp

import (
	"net/http"
	"path/filepath"
	"strconv"

	"github.com/tokibase/tokibase/tools/filesystem"
	"github.com/tokibase/tokibase/tools/list"
)

var inlineServeContentTypes = []string{
	// image
	"image/png", "image/jpg", "image/jpeg", "image/gif", "image/webp", "image/x-icon", "image/bmp",
	// video
	"video/webm", "video/mp4", "video/3gpp", "video/quicktime", "video/x-ms-wmv",
	// audio
	"audio/basic", "audio/aiff", "audio/mpeg", "audio/midi", "audio/mp3", "audio/wave",
	"audio/wav", "audio/x-wav", "audio/x-mpeg", "audio/x-m4a", "audio/aac",
	// document
	"application/pdf", "application/x-pdf",
}

// manualExtensionContentTypes is a map of file extensions to content types.
var manualExtensionContentTypes = map[string]string{
	// https://github.com/whatwg/mimesniff/issues/7
	".svg": "image/svg+xml",

	// https://github.com/gabriel-vasile/mimetype/pull/113
	".css": "text/css",

	// https://github.com/tokibase/tokibase/issues/6597
	".js":  "text/javascript",
	".mjs": "text/javascript",

	// https://github.com/tokibase/tokibase/discussions/7467
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
}

// forceAttachmentParam is the name of the request query parameter to
// force "Content-Disposition: attachment" header.
const forceAttachmentParam = "download"

// Serve serves the file at fileKey location to an HTTP response.
//
// If the `download` query parameter is used the file will be always served for
// download no matter of its type (aka. with "Content-Disposition: attachment").
//
// Internally this function uses [http.ServeContent] so Range requests,
// If-Match, If-Unmodified-Since, etc. headers are handled transparently.
func Serve(s *filesystem.System, res http.ResponseWriter, req *http.Request, fileKey string, name string) error {
	br, readErr := s.GetReader(fileKey)
	if readErr != nil {
		return readErr
	}
	defer br.Close()

	var forceAttachment bool
	if raw := req.URL.Query().Get(forceAttachmentParam); raw != "" {
		forceAttachment, _ = strconv.ParseBool(raw)
	}

	disposition := "attachment"
	realContentType := br.ContentType()
	if !forceAttachment && list.ExistInSlice(realContentType, inlineServeContentTypes) {
		disposition = "inline"
	}

	// make an exception for specific content types and force a custom
	// content type to send in the response so that it can be loaded properly
	extContentType := realContentType
	if ct, found := manualExtensionContentTypes[filepath.Ext(fileKey)]; found {
		extContentType = ct
	}

	setHeaderIfMissing(res, "Content-Disposition", disposition+"; filename="+strconv.Quote(name))
	setHeaderIfMissing(res, "Content-Type", extContentType)
	setHeaderIfMissing(res, "Content-Security-Policy", "default-src 'none'; media-src 'self'; style-src 'unsafe-inline'; sandbox")

	// set a default cache-control header
	// (valid for 30 days but the cache is allowed to reuse the file for any requests
	// that are made in the last day while revalidating the res in the background)
	setHeaderIfMissing(res, "Cache-Control", "max-age=2592000, stale-while-revalidate=86400")

	http.ServeContent(res, req, name, br.ModTime(), br)

	return nil
}

// note: expects key to be in a canonical form (eg. "accept-encoding" should be "Accept-Encoding").
func setHeaderIfMissing(res http.ResponseWriter, key string, value string) {
	if _, ok := res.Header()[key]; !ok {
		res.Header().Set(key, value)
	}
}

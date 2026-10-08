//go:build !no_thumbs

package filesystem

import (
	"errors"
	"image"
	"strconv"

	"github.com/disintegration/imaging"
	"github.com/tokibase/tokibase/tools/filesystem/blob"

	// manually register the webp decoder because disintegration/imaging does not support webp
	_ "golang.org/x/image/webp"
)

func (s *System) createThumb(originalKey, thumbKey, thumbSize string) error {
	sizeParts := ThumbSizeRegex.FindStringSubmatch(thumbSize)
	if len(sizeParts) != 4 {
		return errors.New("thumb size must be in WxH, WxHt, WxHb or WxHf format")
	}

	width, _ := strconv.Atoi(sizeParts[1])
	height, _ := strconv.Atoi(sizeParts[2])
	resizeType := sizeParts[3]

	if width == 0 && height == 0 {
		return errors.New("thumb width and height cannot be zero at the same time")
	}

	// fetch the original
	r, readErr := s.GetReader(originalKey)
	if readErr != nil {
		return readErr
	}
	defer r.Close()

	// create imaging object from the original reader
	// (note: only the first frame for animated image formats)
	img, decodeErr := imaging.Decode(r, imaging.AutoOrientation(true))
	if decodeErr != nil {
		return decodeErr
	}

	var thumbImg *image.NRGBA

	if width == 0 || height == 0 {
		// force resize preserving aspect ratio
		thumbImg = imaging.Resize(img, width, height, imaging.Linear)
	} else {
		switch resizeType {
		case "f":
			// fit
			thumbImg = imaging.Fit(img, width, height, imaging.Linear)
		case "t":
			// fill and crop from top
			thumbImg = imaging.Fill(img, width, height, imaging.Top, imaging.Linear)
		case "b":
			// fill and crop from bottom
			thumbImg = imaging.Fill(img, width, height, imaging.Bottom, imaging.Linear)
		default:
			// fill and crop from center
			thumbImg = imaging.Fill(img, width, height, imaging.Center, imaging.Linear)
		}
	}

	originalContentType := r.ContentType()

	opts := &blob.WriterOptions{
		ContentType: originalContentType,
	}

	var format imaging.Format

	switch originalContentType {
	case "image/jpeg":
		format = imaging.JPEG
	case "image/gif":
		format = imaging.GIF
	case "image/tiff":
		format = imaging.TIFF
	case "image/bmp":
		format = imaging.BMP
	default:
		// fallback to PNG (this includes webp!)
		opts.ContentType = "image/png"
		format = imaging.PNG
	}

	// open a thumb storage writer (aka. prepare for upload)
	w, err := s.NewWriter(thumbKey, opts)
	if err != nil {
		return err
	}

	// thumb encode (aka. upload)
	err = imaging.Encode(w, thumbImg, format)
	if err != nil {
		w.Close()
		return err
	}

	// check for close errors to ensure that the thumb was really saved
	return w.Close()
}

//go:build no_s3fs

package fshttp

import (
	"errors"

	"github.com/tokibase/tokibase/tools/filesystem"
)

// NewS3 is the no_s3fs stub: the S3 driver is compiled out (nano), only the
// local file system is available.
func NewS3(
	bucketName string,
	region string,
	endpoint string,
	accessKey string,
	secretKey string,
	s3ForcePathStyle bool,
) (*filesystem.System, error) {
	return nil, errors.New("S3 storage is not available in this build (no_s3fs)")
}

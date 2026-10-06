package fshttp

import (
	"github.com/tokibase/tokibase/tools/filesystem"
	"github.com/tokibase/tokibase/tools/filesystem/internal/s3blob"
	"github.com/tokibase/tokibase/tools/filesystem/internal/s3blob/s3"
)

// NewS3 initializes a new S3 filesystem instance.
//
// NB! Make sure to call `Close()` after you are done working with it.
func NewS3(
	bucketName string,
	region string,
	endpoint string,
	accessKey string,
	secretKey string,
	s3ForcePathStyle bool,
) (*filesystem.System, error) {
	client := &s3.S3{
		Bucket:       bucketName,
		Region:       region,
		Endpoint:     endpoint,
		AccessKey:    accessKey,
		SecretKey:    secretKey,
		UsePathStyle: s3ForcePathStyle,
	}

	drv, err := s3blob.New(client)
	if err != nil {
		return nil, err
	}

	return filesystem.NewFromDriver(drv), nil
}

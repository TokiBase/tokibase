//go:build replica_s3

package walreplica

import (
	_ "github.com/benbjohnson/litestream/s3" // registers the s3:// replica client (pulls the AWS SDK, ~10 MB)
)

const s3Supported = true

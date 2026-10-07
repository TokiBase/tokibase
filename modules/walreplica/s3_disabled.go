//go:build !replica_s3 && !no_replica

package walreplica

// The s3:// backend is only linked when the binary is built with
// `-tags replica_s3`, because the AWS SDK it needs adds about 10 MB.
// Default builds support file:// replicas only.
const s3Supported = false

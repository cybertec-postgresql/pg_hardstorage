package s3

// SetMultipartForTest lowers the multipart cut-over and part size so a
// small object exercises the multipart path.
func SetMultipartForTest(threshold, minPart int64) (restore func()) {
	t, m := multipartThreshold, minPartSize
	multipartThreshold, minPartSize = threshold, minPart
	return func() { multipartThreshold, minPartSize = t, m }
}

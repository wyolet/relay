package inference

import (
	"io"
	"net/http"
)

// streamCopy pipes src→dst calling Flush after every Write. Used by
// byte-pass paths (runBytePass, handleProxy) so SSE chunks reach the
// caller as they arrive instead of sitting behind Go's default
// http.ResponseWriter buffer (~4 KB).
//
// For non-streaming responses the extra Flush calls are harmless;
// avoiding the branch keeps the code path single and simple. Returns
// the number of bytes copied + any read/write error io.Copy would
// have returned.
func streamCopy(dst http.ResponseWriter, src io.Reader) (int64, error) {
	flusher, _ := dst.(http.Flusher)
	buf := make([]byte, 32*1024)
	var written int64
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			wn, werr := dst.Write(buf[:n])
			written += int64(wn)
			if flusher != nil {
				flusher.Flush()
			}
			if werr != nil {
				return written, werr
			}
			if wn < n {
				return written, io.ErrShortWrite
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				return written, nil
			}
			return written, rerr
		}
	}
}

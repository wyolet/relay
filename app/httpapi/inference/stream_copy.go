package inference

import (
	"io"
	"net/http"
	"sync"
)

// streamCopyBufPool recycles the per-request 32 KB copy buffer. The buffer
// never escapes streamCopy (bytes are written straight to the caller and not
// retained), so it is safe to pool — unlike the pipeline's non-streamed tee
// buffer, which escapes into the post-flight goroutine.
var streamCopyBufPool = sync.Pool{
	New: func() any { b := make([]byte, 32*1024); return &b },
}

// scannerBufPool recycles the 64 KB initial buffer for streamCanonical's
// bufio.Scanner. Frame bytes are copied out of the scanner before the buffer
// is returned, so nothing aliases it after reuse. If a single frame exceeds
// 64 KB the scanner allocates its own larger buffer (up to the 1 MB cap) and
// abandons this one — we still recycle the original 64 KB slice.
var scannerBufPool = sync.Pool{
	New: func() any { b := make([]byte, 64*1024); return &b },
}

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
	bufp := streamCopyBufPool.Get().(*[]byte)
	defer streamCopyBufPool.Put(bufp)
	buf := *bufp
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

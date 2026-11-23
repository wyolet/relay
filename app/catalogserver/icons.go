package catalogserver

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"io/fs"
	"path"
	"regexp"
	"strings"
)

// iconsDir is where a data tarball keeps the files icon.path names: an
// entity's "/provider/<file>" is the tarball's iconsDir + "<file>".
const iconsDir = "data/icons/"

// maxIconBytes bounds one icon read out of a tarball.
const maxIconBytes = 1 << 20

var iconFileName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

var iconContentTypes = map[string]string{
	".png":  "image/png",
	".svg":  "image/svg+xml",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".webp": "image/webp",
}

// iconContentType is the media type of an icon file name; ok is false for a
// name that is not a plain image file name.
func iconContentType(file string) (contentType string, ok bool) {
	if !iconFileName.MatchString(file) {
		return "", false
	}
	contentType, ok = iconContentTypes[strings.ToLower(path.Ext(file))]
	return contentType, ok
}

// archiveIcon reads one icon out of a data tarball, or returns
// fs.ErrNotExist.
func archiveIcon(archive []byte, file string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fs.ErrNotExist
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag != tar.TypeReg || hdr.Name != iconsDir+file {
			continue
		}
		if hdr.Size > maxIconBytes {
			return nil, errors.New("icon exceeds the size limit")
		}
		return io.ReadAll(io.LimitReader(tr, maxIconBytes))
	}
}

package export

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
)

// gzipFile compresses src into dst. qemu-img has no gzip output, so the
// raw.gz format is produced in two steps; doing it in-process rather than
// shelling out to gzip keeps the adapter working on a host that has qemu but
// no gzip binary, and lets a caller's cancellation take effect between steps.
func gzipFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	zw := gzip.NewWriter(out)
	if _, err := io.Copy(zw, in); err != nil {
		zw.Close()
		return fmt.Errorf("compressing %s: %w", src, err)
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("finishing %s: %w", dst, err)
	}
	return out.Close()
}

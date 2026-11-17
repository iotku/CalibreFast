package kosync

import (
	"crypto/md5"
	"encoding/hex"
	"io"
	"os"
)

// PartialMD5 is KOReader's document id for a file's contents
// (frontend/util.lua, util.partialMD5): the MD5 of 1 KiB samples taken at
// offsets 0, 1 KiB, 4 KiB, 16 KiB, … 1 GiB, stopping at the first one at or
// beyond the end of the file. The last sample may be short.
//
// KOReader computes the offsets as bit.lshift(1024, 2*i) for i = -1..10.
// LuaJIT masks the shift count to five bits, so i = -1 shifts 1024 left by
// 30, which overflows 32 bits to 0: the first sample is at 0, not 256.
func PartialMD5(r io.ReaderAt) (string, error) {
	h := md5.New()
	buf := make([]byte, 1024)
	for i := -1; i <= 10; i++ {
		var off int64
		if i >= 0 {
			off = 1024 << (2 * i)
		}
		n, err := r.ReadAt(buf, off)
		if n == 0 {
			if err != nil && err != io.EOF {
				return "", err
			}
			break // at or past the end
		}
		h.Write(buf[:n])
		if err != nil && err != io.EOF {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// PartialMD5File is PartialMD5 of the file at path.
func PartialMD5File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return PartialMD5(f)
}

// FileNameDigest is KOReader's id under the filename matching method.
func FileNameDigest(name string) string {
	sum := md5.Sum([]byte(name))
	return hex.EncodeToString(sum[:])
}

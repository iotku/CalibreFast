package epubzip

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"strings"

	"github.com/iotku/CalibreFast/pkg/xml11"
)

// PackagePath is the path of an EPUB's package document (the OPF):
// from META-INF/container.xml, else the first .opf in the archive.
func PackagePath(zr *zip.Reader) (string, error) {
	if data, err := ReadEntry(zr, "META-INF/container.xml", 1<<20); err == nil {
		var c struct {
			Rootfiles []struct {
				FullPath string `xml:"full-path,attr"`
			} `xml:"rootfiles>rootfile"`
		}
		if xml11.Unmarshal(data, &c) == nil && len(c.Rootfiles) > 0 && c.Rootfiles[0].FullPath != "" {
			return c.Rootfiles[0].FullPath, nil
		}
	}
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, ".opf") {
			return f.Name, nil
		}
	}
	return "", errors.New("could not find OPF in epub")
}

// ReadEntry reads one file from a zip, refusing entries over max bytes.
func ReadEntry(zr *zip.Reader, name string, max int64) ([]byte, error) {
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		data, err := io.ReadAll(io.LimitReader(rc, max+1))
		if err != nil {
			return nil, err
		}
		if int64(len(data)) > max {
			return nil, fmt.Errorf("%s is larger than %d bytes", name, max)
		}
		return data, nil
	}
	return nil, fmt.Errorf("%s not found", name)
}

// HrefPath resolves a manifest href against the package document.
func HrefPath(opfPath, href string) string {
	href, _, _ = strings.Cut(href, "#")
	if u, err := url.PathUnescape(href); err == nil {
		href = u
	}
	return path.Join(path.Dir(opfPath), href)
}

// manifest is the part of an EPUB's package document that names its cover.
type manifest struct {
	Metas []struct {
		Name    string `xml:"name,attr"`
		Content string `xml:"content,attr"`
	} `xml:"metadata>meta"`
	Items []struct {
		ID         string `xml:"id,attr"`
		Href       string `xml:"href,attr"`
		MediaType  string `xml:"media-type,attr"`
		Properties string `xml:"properties,attr"`
	} `xml:"manifest>item"`
}

// CoverHref is the zip path of the cover image an EPUB declares: EPUB 3's
// cover-image item, else EPUB 2's <meta name="cover">, else an image item
// whose id is "cover" or "cover-image".
func CoverHref(opfPath string, opfData []byte) (string, error) {
	var m manifest
	if err := xml11.Unmarshal(opfData, &m); err != nil {
		return "", fmt.Errorf("parsing the OPF: %w", err)
	}
	isImage := func(mt string) bool { return strings.HasPrefix(strings.ToLower(mt), "image/") }
	href := ""
	for _, it := range m.Items {
		if isImage(it.MediaType) && strings.Contains(" "+it.Properties+" ", " cover-image ") {
			href = it.Href
			break
		}
	}
	if href == "" {
		coverID := ""
		for _, meta := range m.Metas {
			if strings.EqualFold(meta.Name, "cover") {
				coverID = meta.Content
				break
			}
		}
	ids:
		for _, id := range []string{coverID, "cover", "cover-image"} {
			if id == "" {
				continue
			}
			for _, it := range m.Items {
				if it.ID == id && isImage(it.MediaType) {
					href = it.Href
					break ids
				}
			}
		}
	}
	if href == "" {
		return "", errors.New("the EPUB declares no cover")
	}
	if i := strings.IndexAny(href, "?#"); i >= 0 {
		href = href[:i]
	}
	href = strings.ReplaceAll(href, "%20", " ")
	return path.Clean(path.Join(path.Dir(opfPath), href)), nil
}

// CoverData opens the EPUB at path p and returns the image file it declares as
// its cover (see [CoverHref]), as it is stored. An image larger than maxBytes
// is refused.
func CoverData(p string, maxBytes int64) ([]byte, error) {
	zr, err := zip.OpenReader(p)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	opfPath, err := PackagePath(&zr.Reader)
	if err != nil {
		return nil, err
	}
	opfData, err := ReadEntry(&zr.Reader, opfPath, 16<<20)
	if err != nil {
		return nil, err
	}
	href, err := CoverHref(opfPath, opfData)
	if err != nil {
		return nil, err
	}
	data, err := ReadEntry(&zr.Reader, href, maxBytes)
	if err != nil {
		return nil, err
	}
	return data, nil
}

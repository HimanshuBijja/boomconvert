package main

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePNG(t *testing.T, path string) {
	t.Helper()
	// Minimal valid PNG header + IHDR chunk so filetype.Match identifies it.
	data := []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, // PNG signature
		0x00, 0x00, 0x00, 0x0D, // IHDR length
		0x49, 0x48, 0x44, 0x52, // "IHDR"
		0x00, 0x00, 0x00, 0x01, // width 1
		0x00, 0x00, 0x00, 0x01, // height 1
		0x08, 0x06, 0x00, 0x00, 0x00, // bit depth/color/etc
		0x1F, 0x15, 0xC4, 0x89, // crc
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeOOXML(t *testing.T, path, family string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	prefix := map[string]string{"docx": "word/", "pptx": "ppt/", "xlsx": "xl/"}[family]
	if prefix == "" {
		t.Fatalf("unknown family %s", family)
	}
	w, err := zw.Create(prefix + "document.xml")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("<xml/>"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

func TestDetectFormat_PNGViaMagicBytes(t *testing.T) {
	p := filepath.Join(t.TempDir(), "file.bin")
	writePNG(t, p)
	got := detectFormatWithFallback(p, "jpg")
	if got != "png" {
		t.Fatalf("expected png, got %q", got)
	}
}

func TestDetectFormat_OOXMLDisambiguation(t *testing.T) {
	cases := []string{"docx", "pptx", "xlsx"}
	for _, family := range cases {
		t.Run(family, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "file.bin")
			writeOOXML(t, p, family)
			// Fallback ext intentionally bogus so we know detection (not fallback) won.
			got := detectFormatWithFallback(p, "bogus")
			if got != family {
				t.Fatalf("expected %s, got %q", family, got)
			}
		})
	}
}

func TestDetectFormat_FallsBackToOriginalExt(t *testing.T) {
	p := filepath.Join(t.TempDir(), "file.bin")
	if err := os.WriteFile(p, []byte("not a known format"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := detectFormatWithFallback(p, "txt")
	if got != "txt" {
		t.Fatalf("expected txt fallback, got %q", got)
	}
}

func TestNormalizeExt(t *testing.T) {
	cases := map[string]string{
		"JPG":  "jpg",
		"jpeg": "jpg",
		".png": "png",
		"TIF":  "tiff",
		"tiff": "tiff",
	}
	for in, want := range cases {
		if got := normalizeExt(in); got != want {
			t.Errorf("normalizeExt(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTempSibling_PreservesExtension(t *testing.T) {
	cases := map[string]string{
		filepath.Join("dir", "foo.png"):     ".bc-tmp.png",
		filepath.Join("dir", "report.docx"): ".bc-tmp.docx",
		// Compound names — only the last segment is the extension.
		filepath.Join("dir", "archive.tar.gz"): ".bc-tmp.gz",
	}
	for input, suffix := range cases {
		got := tempSibling(input)
		if !strings.HasSuffix(got, suffix) {
			t.Errorf("tempSibling(%q) = %q, want suffix %q", input, got, suffix)
		}
		if filepath.Dir(got) != filepath.Dir(input) {
			t.Errorf("tempSibling(%q) changed directory: %s", input, got)
		}
	}
}

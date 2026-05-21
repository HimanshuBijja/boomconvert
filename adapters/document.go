package adapters

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// LibreOfficeAdapter: DOCX<->PDF and PPTX<->PDF via headless soffice.
type LibreOfficeAdapter struct{}

func (LibreOfficeAdapter) Name() string         { return "libreoffice" }
func (LibreOfficeAdapter) RequiredTool() string { return "libreoffice" }

func (LibreOfficeAdapter) Supports(src, dst string) bool {
	switch {
	case src == "docx" && dst == "pdf":
		return true
	case src == "pptx" && dst == "pdf":
		return true
	}
	return false
}

func (LibreOfficeAdapter) Convert(ctx context.Context, src, dst string, _ ConvertOptions) error {
	outDir, err := os.MkdirTemp("", "boomconvert-lo-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(outDir)

	profileDir, err := os.MkdirTemp("", "boomconvert-loprofile-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(profileDir)
	userInstallation := "-env:UserInstallation=file:///" + filepath.ToSlash(profileDir)

	dstExt := Canonical(extOf(dst))
	cmd := exec.CommandContext(ctx, "soffice",
		userInstallation,
		"--headless", "--norestore", "--nologo", "--nodefault",
		"--convert-to", dstExt,
		"--outdir", outDir,
		src,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("soffice: %w: %s", err, string(out))
	}
	srcBase := filepath.Base(src)
	srcStem := srcBase[:len(srcBase)-len(filepath.Ext(srcBase))]
	produced := filepath.Join(outDir, srcStem+"."+dstExt)
	if _, err := os.Stat(produced); err != nil {
		return fmt.Errorf("soffice did not produce expected file %s: %s", produced, string(out))
	}
	return moveFile(produced, dst)
}

func (LibreOfficeAdapter) Rules() []Rule {
	return []Rule{
		{Source: "docx", Target: "pdf"},
		{Source: "pptx", Target: "pdf"},
	}
}

// Pdf2DocxAdapter: PDF -> DOCX with high fidelity via python -m pdf2docx.
type Pdf2DocxAdapter struct{}

func (Pdf2DocxAdapter) Name() string         { return "pdf2docx" }
func (Pdf2DocxAdapter) RequiredTool() string { return "pdf2docx" }

func (Pdf2DocxAdapter) Supports(src, dst string) bool {
	return src == "pdf" && dst == "docx"
}

func (Pdf2DocxAdapter) Convert(ctx context.Context, src, dst string, _ ConvertOptions) error {
	cmd := exec.CommandContext(ctx, "python", "-m", "pdf2docx", "convert", src, dst)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("pdf2docx: %w: %s", err, string(out))
	}
	return nil
}

func (Pdf2DocxAdapter) Rules() []Rule {
	return []Rule{{Source: "pdf", Target: "docx"}}
}

func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	// Cross-volume fallback: copy + remove.
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Remove(src)
}

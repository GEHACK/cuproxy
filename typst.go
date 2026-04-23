package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/jung-kurt/gofpdf"
	"github.com/rs/zerolog"
	"github.com/tuupke/utils/env"
)

var (
	typstTemplate = env.String("TYPST_TEMPLATE", "")
	typstBin      = env.String("TYPST_BIN", "typst")
)

func init() {
	if typstTemplate == "" {
		return
	}

	if _, err := os.Stat(typstTemplate); err != nil {
		panic(fmt.Errorf("TYPST_TEMPLATE file '%v' not accessible: %w", typstTemplate, err))
	}

	if _, err := exec.LookPath(typstBin); err != nil {
		panic(fmt.Errorf("TYPST_BIN '%v' not found: %w", typstBin, err))
	}
}

func TypstBannerPage(log zerolog.Logger, outWrite io.Writer, data *Props, _ ...string) error {
	outFile, err := os.CreateTemp("", "cuproxy-typst-out-*.pdf")
	if err != nil {
		return fmt.Errorf("could not create temp output file: %w", err)
	}
	defer os.Remove(outFile.Name())
	outFile.Close()

	args := []string{"compile", typstTemplate, outFile.Name(), "--root", "/"}
	data.Range(func(k, v string) bool {
		args = append(args, "--input", k+"="+v)
		return true
	})

	cmd := exec.Command(typstBin, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Error().Err(err).Str("output", string(output)).Msg("typst compilation failed")
		return fmt.Errorf("typst compile failed: %w", err)
	}

	compiled, err := os.Open(outFile.Name())
	if err != nil {
		return fmt.Errorf("could not open compiled pdf: %w", err)
	}
	defer compiled.Close()

	if bannerOnBack {
		blank, err := blankPage()
		if err != nil {
			return fmt.Errorf("could not create blank page: %w", err)
		}
		defer os.Remove(blank.Name())
		defer blank.Close()

		return stitch([]io.ReadSeeker{blank, compiled}, outWrite, useGhostscript)
	}

	_, err = io.Copy(outWrite, compiled)
	return err
}

func blankPage() (*os.File, error) {
	f, err := os.CreateTemp("", "cuproxy-typst-blank-*.pdf")
	if err != nil {
		return nil, err
	}

	orientation := "P"
	if pdfInLandscape {
		orientation = "L"
	}

	pdf := gofpdf.New(orientation, pdfUnit, pdfSize, "")
	pdf.AddPage()
	if err := pdf.Output(f); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, err
	}

	if _, err := f.Seek(0, 0); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, err
	}

	return f, nil
}

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"

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

type typstData struct {
	Keys []string          `json:"keys"`
	Data map[string]string `json:"data"`
}

func TypstBannerPage(log zerolog.Logger, outWrite io.Writer, data *Props, keys ...string) error {
	if len(keys) == 1 && keys[0] == "*" {
		keys = make([]string, 0, 100)
		data.Range(func(key, _ string) bool {
			keys = append(keys, key)
			return true
		})

		slices.Sort(keys)
	}

	// Build the data structure for the template
	td := typstData{
		Keys: keys,
		Data: make(map[string]string, len(keys)),
	}
	for _, k := range keys {
		if v, ok := data.Load(k); ok {
			td.Data[k] = v
		}
	}

	// Write JSON data to a temp file
	jsonFile, err := os.CreateTemp("", "cuproxy-typst-data-*.json")
	if err != nil {
		return fmt.Errorf("could not create temp json file: %w", err)
	}
	defer os.Remove(jsonFile.Name())

	if err := json.NewEncoder(jsonFile).Encode(td); err != nil {
		jsonFile.Close()
		return fmt.Errorf("could not write json data: %w", err)
	}
	jsonFile.Close()

	// Create temp output PDF
	outFile, err := os.CreateTemp("", "cuproxy-typst-out-*.pdf")
	if err != nil {
		return fmt.Errorf("could not create temp output file: %w", err)
	}
	defer os.Remove(outFile.Name())
	outFile.Close()

	// Compile the typst template. --root / is needed so the template can
	// resolve the absolute path to the JSON data file.
	cmd := exec.Command(typstBin, "compile", typstTemplate, outFile.Name(),
		"--root", "/",
		"--input", "data-path="+jsonFile.Name(),
	)
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
		// Prepend a blank page by stitching a blank PDF before the compiled output
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

// blankPage creates a single blank PDF page and returns an open file seeked to the start.
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

package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/jung-kurt/gofpdf"
	"github.com/rs/zerolog"
	"github.com/tuupke/utils/env"
)

var (
	pdfUnit        = env.String("PDF_UNIT", "mm")
	pdfSize        = env.String("PDF_PAGE_SIZE", "A4")
	pdfFontDir     = env.String("PDF_FONT_DIR", "")
	pdfInLandscape = env.Bool("PDF_LANDSCAPE", false)

	pdfLeftMargin = env.Float("PDF_LEFT_MARGIN", 10)
	pdfTopMargin  = env.Float("PDF_TOP_MARGIN", 10)
	fontSize      = env.Float("PDF_FONT_SIZE", 12)
	pdfLineHeight = pointsToUnits(fontSize) * env.Float("PDF_LINE_HEIGHT", 1.2)
	font          = env.String("PDF_FONT_FAMILY", "Arial")

	imgDpi = float64(env.Int("IMAGE_PPI", 120))
)

func pointsToUnits(points float64) float64 {
	switch pdfUnit {
	case "mm", "":
		return points * 0.352778
	case "cm":
		return points * 0.0352778
	case "pt":
		return points
	case "in":
		return points * 0.0138889
	}

	panic("Unknown pdf unit: " + pdfUnit)
}

func BannerPage(log zerolog.Logger, outWrite io.Writer, data *Props, keys ...string) error {
	if typstTemplate != "" {
		return TypstBannerPage(log, outWrite, data, keys...)
	}
	if len(keys) == 1 && keys[0] == "*" {
		keys = make([]string, 0, 100)
		data.Range(func(key, _ string) bool {
			keys = append(keys, key)
			return true
		})

		slices.Sort(keys)
	}

	orientation := "P"
	if pdfInLandscape {
		orientation = "L"
	}

	log.Info().
		Bool("landscape", pdfInLandscape).
		Int("num_keys", len(keys)).
		Msg("rendering new banner")

	pdf := gofpdf.New(orientation, pdfUnit, pdfSize, pdfFontDir)
	if bannerOnBack {
		pdf.AddPage()
	}

	pdf.AddPage()
	pdf.SetFont(font, "", fontSize)

	yTop := pdfTopMargin
	for _, k := range keys {
		val, ok := data.Load(k)
		if !ok {
			continue
		}

		if slices.Contains(imageKeys, k) && val != "" {
			if err := renderImage(log, pdf, k, val, &yTop); err != nil {
				log.Err(err).Str("key", k).Str("value", val).Msg("could not render image, skipping")
			}
			continue
		}

		pdf.Text(pdfLeftMargin, yTop+pdfLineHeight, fmt.Sprintf("%v: %v", k, val))
		yTop += pdfLineHeight
	}

	return pdf.Output(outWrite)
}

func renderImage(log zerolog.Logger, pdf *gofpdf.Fpdf, key, path string, yTop *float64) error {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))
	if ext == "jpeg" {
		ext = "jpg"
	}
	if ext != "jpg" && ext != "png" && ext != "gif" {
		return fmt.Errorf("unsupported image extension %q for %q", ext, path)
	}

	image, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open image %q: %w", path, err)
	}
	defer image.Close()

	iopts := gofpdf.ImageOptions{ReadDpi: true, ImageType: ext}
	info := pdf.RegisterImageOptionsReader(key, iopts, image)
	if info == nil {
		if pdfErr := pdf.Error(); pdfErr != nil {
			return fmt.Errorf("gofpdf rejected image %q: %w", path, pdfErr)
		}
		return fmt.Errorf("gofpdf returned nil image info for %q", path)
	}

	info.SetDpi(imgDpi)

	pageW, _ := pdf.GetPageSize()
	maxW := pageW - 2*pdfLeftMargin
	w, h := info.Width(), info.Height()
	if w > maxW {
		h = h * maxW / w
		w = maxW
	}

	pdf.ImageOptions(key, pdfLeftMargin, *yTop, w, h, true, iopts, 0, "")
	*yTop += h + pdfLineHeight

	log.Debug().
		Str("key", key).
		Str("path", path).
		Float64("drawn_w", w).
		Float64("drawn_h", h).
		Float64("y", *yTop).
		Msg("rendered image")
	return nil
}

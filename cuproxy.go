package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime/debug"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/chebyrash/promise"
	"github.com/fasthttp/router"
	"github.com/joho/godotenv"
	pdfcpu "github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/puzpuzpuz/xsync"
	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"
	"github.com/tuupke/utils/env"
	"github.com/tuupke/utils/lifecycle"
	"github.com/valyala/fasthttp"
)

var (
	cupsListen = env.String("LISTEN", ":631")
	printerTo  = env.String("PRINTER_TO", "")

	to = btsReplace([]byte("ipp://" + printerTo))

	dumpsPath        = env.String("DUMP_IPP_CONTENTS", "")
	dumpReplacements = env.Bool("DUMP_REPLACEMENTS", false)
	dumpOriginal     = env.Bool("DUMP_ORIGINAL", false)
	appendBanner     = env.Bool("BANNER_APPEND", false)
	bannerOnBack     = env.Bool("BANNER_ON_BACK", false)
	useGhostscript   = env.Bool("USE_GHOSTSCRIPT", false)
	cupsfilter       = env.String("CUPSFILTER_LOCATION", "")
	maxRequestSize   = 128 << 20 // env.Int("MAX_REQUEST_SIZE", 128<<20) // 128 MiB
	seqId            = new(uint64)
	numPrints        = new(uint64)
	ppdLocation      = env.String(
		"PPD_LOCATION",
		"/usr/share/ppd/cupsfilters/Generic-PDF_Printer-PDF.ppd",
	)

	panicWithoutBanner = env.Bool("BANNER_MUST_EXIST", false)

	pdfLocation = strings.TrimRight(env.String("PDF_LOCATION", os.TempDir()), "/")

	// requestPromises maps every CUPS job-id to the promise that will resolve
	// to the banner PDF for that job. Populated on the CreateJob (0x05) request
	// and awaited on the subsequent PrintJob (0x02 / 0x06) request.
	requestPromises = xsync.NewIntegerMapOf[int32, *promise.Promise[*os.File]]()
)

func init() {
	// Load the logger
	l, err := zerolog.ParseLevel(env.String("LOG_LEVEL", "info"))
	if err != nil {
		zlog.Fatal().Err(err).Msg("could not parse level")
	}

	zlog.Info().Msg("using loglevel " + l.String())
	zerolog.SetGlobalLevel(l)
	err = godotenv.Load()
	if err != nil {
		zlog.Info().Msg("error" + err.Error())
	}
}

func main() {
	if err := os.MkdirAll(pdfLocation, 0o755); err != nil {
		zlog.Fatal().Err(err).Str("pdf-folder", pdfLocation).Msg("cannot create pdf-folder")
	}

	routes := router.New()
	routes.PanicHandler = func(ctx *fasthttp.RequestCtx, i any) {
		zlog.Error().Interface("error", i).Msg("received panic")
		debug.PrintStack()
	}

	routes.ANY("/{path:*}", cupsHandler)

	// Handle dumpinng of requests
	if dumpsPath != "" && (dumpReplacements || dumpOriginal) {
		dumpsPath += "/dumps"
		// Recreate the dumps folder by recreating it entirely.
		if err := os.RemoveAll(dumpsPath); err != nil && !os.IsNotExist(err) {
			zlog.Fatal().Err(err).Str("path", dumpsPath).Msg("could not delete dumps folder")
		}
		zlog.Info().Str("path", dumpsPath).Msg("deleted previous dumps folder")

		if err := os.MkdirAll(dumpsPath, 0o755); err != nil {
			zlog.Fatal().Str("path", dumpsPath).Err(err).Msg("could not create dumps folder")
		}
		zlog.Info().Msg("created dumps folder")
	}

	// Create and start the webserver
	server := fasthttp.Server{
		Handler:            routes.Handler,
		MaxRequestBodySize: maxRequestSize,
	}

	ln, err := net.Listen("tcp4", cupsListen)
	if err != nil {
		zlog.Fatal().Err(err).Msg("cups proxy cannot be started")
	}

	lifecycle.CleanupErr(ln.Close)
	zlog.Info().Msg("started cups proxy")
	go server.Serve(ln)

	zlog.Info().
		Str("printer to", printerTo).
		Str("listen", cupsListen).
		Int("max_body_size", maxRequestSize).
		Msg("Booted")
	lifecycle.Cleanup(func() { zlog.Warn().Msg("Stopping") })
	lifecycle.AwaitStop()
}

// Byter is an interface around the `Bytes` method returning a byte slice. Used
// for more consistent logging of request/response contents.
type (
	Byter interface {
		Bytes() []byte
	}
	byteSlice []byte
)

func (b byteSlice) Bytes() []byte {
	return b
}

func writeToFile[T Byter](seqId uint64, request, replaced bool, contents T) error {
	if dumpsPath == "" || (replaced && !dumpReplacements) || (!replaced && !dumpOriginal) {
		return nil
	}

	req, typ := "res", "orig"
	if request {
		req = "req"
	}

	if replaced {
		typ = "repl"
	}

	name := fmt.Sprintf("%v/%v-%v-%v.bin", dumpsPath, seqId, req, typ)
	f, err := os.OpenFile(name, os.O_CREATE|os.O_RDWR, 0o755)
	if err != nil {
		return fmt.Errorf("could not open dump-file '%v'; %w", name, err)
	}

	defer f.Close()
	if _, err := f.Write(contents.Bytes()); err != nil {
		return fmt.Errorf("could not write to dump-file '%v'; %w", name, err)
	}

	return nil
}

func stitchPdfCpu(toStitch []io.ReadSeeker, out io.Writer) (err error) {
	return pdfcpu.MergeRaw(toStitch, out, false, nil)
}

func stitchGhostscript(toStitch []io.ReadSeeker, out io.Writer) (err error) {
	// Attempt to convert all toStitch's to a file
	fmt.Println("stitching ghostscript")
	filenames := make([]string, len(toStitch))

	for i := range filenames {
		var f *os.File
		var ok bool

		if f, ok = toStitch[i].(*os.File); ok {
			filenames[i] = f.Name()
			continue
		}

		// Create a temporary file to write to
		f, err = os.CreateTemp("", "cuproxy-gs-stitch-")
		if err != nil {
			return err
		}

		// Delete all files when done. Don't care about the loop, should be small.
		defer func(f *os.File) {
			_ = os.Remove(f.Name())
		}(f)

		var n int64
		n, err = io.Copy(f, toStitch[i])
		if err != nil {
			return err
		}

		fmt.Println("copied ", n, " bytes to", f.Name())
		filenames[i] = f.Name()

		err = f.Close()
		if err != nil {
			return err
		}
	}

	// Create the output file
	outFile, err := os.CreateTemp("", "cuproxy-gs-stitch-out-")
	if err != nil {
		return err
	}
	defer os.Remove(outFile.Name())

	// Invoke ghostscript
	args := append([]string{
		"-q",
		"-dCompatibilityLevel=1.4",
		"-dNOPAUSE",
		"-dQUIET",
		"-dBATCH",
		"-sDEVICE=pdfwrite",
		"-sOutputFile=" + outFile.Name(),
	}, filenames...)

	var bts []byte
	bts, err = exec.Command("gs", args...).CombinedOutput()
	fmt.Println(string(bts))
	if err != nil {
		return
	}

	_, err = outFile.Seek(0, 0)
	if err != nil {
		return
	}

	_, err = io.Copy(out, outFile)
	return
}

func stitch(toStitch []io.ReadSeeker, out io.Writer, ghostscript bool) error {
	if !ghostscript {
		return stitchPdfCpu(toStitch, out)
	}

	return stitchGhostscript(toStitch, out)
}

func cupsHandler(ctx *fasthttp.RequestCtx) {
	seqId := atomic.AddUint64(seqId, 1)
	atomic.AddUint64(numPrints, 1)
	body := slices.Clone(ctx.Request.Body())

	// Extract the type of operation. Consider extracting operation into a custom type for improved readability.
	var operationId byte
	if len(body) > 3 {
		operationId = body[3]
	}
	var jobId int32
	isCreate := operationId == 0x05
	isPrint := operationId == 0x02 || operationId == 0x06

	// Construct a logger
	path := bytes.Trim(ctx.Request.URI().Path(), "/")
	requestedUrl := fmt.Sprintf("ipp://%s/%s", cupsListen, path)
	log := zlog.With().
		IPAddr("ip", ctx.RemoteIP()).
		Str("url", requestedUrl).
		Uint64("seq-id", seqId).
		Logger()

	// Replace the url in the body, the actual printer's url is constant so does not
	// need to be rebuilt every request.
	from := btsReplace([]byte(requestedUrl))
	log.Debug().
		Err(writeToFile[byteSlice](seqId, true, false, body)).
		Msg("written original request")
	body = bytes.ReplaceAll(body, from, to)

	var b *bytes.Buffer
	if !isPrint {
		// Base case, simply proxy the entire request.
		b = bytes.NewBuffer(body)
	} else {
		// An actual print job.

		// Retrieve (or start) the banner-PDF promise for this job.
		var bannerPromise *promise.Promise[*os.File]
		var found bool
		jobId, found = extractInt("job-id", body)
		log := log.With().Int32("job-id", jobId).Bool("job-id-found", found).Logger()
		log.Info().Msg("print triggered")

		if !found {
			log.Error().Msg("got print and cannot extract job-id, unusual")
			bannerPromise = loadValues(log, ctx, jobId)
		} else {
			bannerPromise, found = requestPromises.Load(jobId)
			if !found {
				log.Warn().Msg("did not find promise for job, i.e. job is unknown to proxy; trying to load new data")
				bannerPromise = loadValues(log, ctx, jobId)
			}
		}

		// Handling a print job is not trivial. There are two 'problematic' issues to account for:
		//  1. The client might (and is allowed to) ignore that 'application/pdf' is the
		//     only supported mime-type. i.e. conversion from some mime-type to 'application/pdf'
		//     must be supported.
		//  2. PJL can 'wrap' print-jobs. i.e. PJL must be detected and unwrapped when
		//    used. It is possible that after conversion, the job is in PJL again.
		//
		// Conversion to application/pdf is a whole can of worms. `cupsfilters` and an
		// appropriate ppd are used to do this step.
		//
		// The rendered banner-page is then stitched to the to-be-printed PDF using
		// PDFCPU, then passed to the actual printer.

		// Extract the to-be-printed file. This file is at the end of the IPP request,
		// but might be a PJL job.
		startOfData, err := dataOffset(body)
		log.Debug().Err(err).Int("data_start", startOfData).Msg("found offset")
		if err != nil {
			log.Panic().Err(err).Int("data_start", startOfData).Msg("could not extract start of data")
			panic(err)
		}

		// Create a new body buffer and keep the IPP preamble.
		// Do not handle the thrown error even though the job can now fail.
		newB := bytes.NewBuffer(make([]byte, 0, len(body)+startOfData+2048))
		num, err := newB.Write(body[:startOfData])
		log.Trace().Err(err).Int("num", num).Msg("written preamble of request to new body")
		if err != nil {
			log.Err(err).Int("num", num).Msg("written preamble of request to new body")
		}

		// Extract the
		contents := make([]byte, len(body)-startOfData)
		copy(contents, body[startOfData:])

		var prefix, suffix []byte

		oLen := len(contents)
		// Body might contain PJL, needs to be kept but stripped
		prefix, suffix, contents, err = extractPJL(contents)
		log.Err(err).
			Int("prefix_len", len(prefix)).
			Int("suffix_len", len(suffix)).
			Int("contents_len", len(contents)).
			Int("original_len", oLen).
			Msg("extracted PJL body from print-job")

		num, err = newB.Write(prefix)
		log.Trace().Err(err).Int("num", num).Msg("written prefix of request to new body")
		if err != nil {
			log.Err(err).Int("num", num).Msg("could not write prefix of request to new body")
		}

		// PDF start with "%PDF" and end with "%%EOF"
		if pdfStart := bytes.Index(contents, []byte("%PDF")); pdfStart < 0 {
			contents, err = cupsConvert(log, contents, "application/pdf", ppdLocation)
			log.Err(err).Msg("converted contents to PDF")
			if err != nil {
				log.Panic().Msg("conversion to pdf is required")
			}

			// Might contain pjl. If so, strip away the PJL.
			_, _, contents, err = extractPJL(contents)
			if err != nil {
				log.Err(err).Msg("could not unwrap converted pdf from PJL")
			}
		}

		// It must now hold that `contents` contains a PDF.
		pdfReader := bytes.NewReader(contents)

		// The to-be-printed document is ready; wait for the banner PDF. This
		// blocks until every webhook has finished — we do not render with
		// partial data.
		filePointer, err := bannerPromise.Await(lifecycle.Context())
		log.Err(err).Msg("retrieved PDF to stitch")

		// If no banner page exists (or its promise failed), fall back to
		// proxying the original print unmodified.
		if filePointer == nil {
			if panicWithoutBanner {
				log.Panic().Msg("no banner, aborting")
			}
			log.Warn().Msg("no banner available, forwarding original print unchanged")
			b = bytes.NewBuffer(body)
		} else {
			file := *filePointer
			defer file.Close()

			tempReader := bytes.NewBuffer(make([]byte, 0, len(body)))

			tf, _ := os.CreateTemp("", "cuproxy-stitched-*.pdf")
			mw := io.MultiWriter(tempReader, tf)

			pdf := []io.ReadSeeker{file, pdfReader}
			if appendBanner {
				// Flip the pdfs to stitch
				pdf[0], pdf[1] = pdf[1], pdf[0]
			}

			err := stitch(pdf, mw, useGhostscript)
			log.Err(err).Msg("merged banner with main print")
			if err == nil {
				num, err := io.Copy(newB, tempReader)
				log.Debug().Err(err).Int64("num", num).Msg("copied buffer")
			} else {
				io.Copy(newB, pdfReader)
			}

			// Write the rest of the original PJL description (if it exists), and replace
			// what will be sent to the actual printer.
			num, err = newB.Write(suffix)
			log.Trace().Err(err).Int("num", num).Msg("written rest of request to new body")
			b = newB
		}
	}

	log.Trace().Err(writeToFile(seqId, true, true, b)).Msg("written replaced request")

	// Construct the proxy request, since `b` is always a *bytes.Buffer and these get closed automatically
	proxiedRequest, err := http.NewRequest(string(ctx.Method()), "http://"+printerTo, b)
	log.Debug().Err(err).Msg("created request to proxy")
	ctx.Request.Header.VisitAll(func(key, value []byte) {
		proxiedRequest.Header.Add(
			string(key),
			strings.ReplaceAll(string(value), requestedUrl[5:], printerTo),
		)
	})

	resp, err := http.DefaultClient.Do(proxiedRequest)
	log.Debug().Err(err).Msg("proxied request")
	err = proxiedRequest.Body.Close()
	log.Debug().Err(err).Msg("closed request body")
	if err != nil {
		panic(err)
	}

	body, err = io.ReadAll(resp.Body)
	log.Debug().Err(err).Msg("read entire response body")
	err = resp.Body.Close()
	log.Debug().Err(err).Msg("closed response body")
	body = setReplace(body)

	ctx.SetStatusCode(resp.StatusCode)
	for k, values := range resp.Header {
		for _, value := range values {
			repl := strings.ReplaceAll(value, printerTo, requestedUrl[5:])
			ctx.Response.Header.Add(k, repl)
		}
	}

	log.Trace().
		Err(writeToFile[byteSlice](seqId, false, false, body)).
		Msg("written original response")
	body = bytes.ReplaceAll(body, to, from)
	log.Debug().Msg("replaced response body")
	log.Trace().
		Err(writeToFile[byteSlice](seqId, false, true, body)).
		Msg("written replaced response")

	if isCreate {
		var found bool
		jobId, found = extractInt("job-id", body)
		if !found {
			// Weirdness happens here
			log.Warn().Msg("cannot deduce job-id though it should be present!")
		} else {
			requestPromises.Store(jobId, loadValues(log, ctx, jobId))
		}
	}

	// Write the body
	if _, err := ctx.Write(body); err != nil {
		panic(err)
	}

	log.Debug().Msg("written proxied-body")
}

// btsReplace takes a byte string and converts it to the representation used in CUPS
func btsReplace(check []byte) []byte {
	checkLen := len(check)
	b := make([]byte, 2+checkLen)
	copy(b[2:], check)

	// Prepend big-endian length, it's encoded in two bytes
	binary.BigEndian.PutUint16(b, uint16(checkLen))
	return b
}

// setReplace replaces all relevant sets/collections in the response that are
// needed to convince cups that only PDF is supported. All properties starting
// with "document-format-" need to be replaced. To simplify even further, all
// properties that depict mime-types are replaced.
func setReplace(body []byte) []byte {
	// The format used by CUPS is a bit annoying to efficiently replace. For a single value:
	//   <type, 1B><length, 2B, big-endian><key-name, `length`B><value length, 2B,
	//   big-endian><value, `value length`B>
	// subsequent values are than 'appended' as follows:
	//   <type, 1B>\u0000\u0000<value length, 2B, big-endian><value, `value length`B>
	// The two nulls represent the 'empty key' and thus the value should be interpreted as being part of last key.

	// All keys storing mime-types should be set to 'application/pdf'. The type depicting a mime-type is "I"
	result := make([]byte, len(body))
	// We know all our properties start with 0x49 0x00
	prefix := []byte("I\u0000")

	var matchedUntil, appendedUntil int
	pdfMime := "\u0000\u000Fapplication/pdf"

	for len(body) > matchedUntil {
		next := bytes.Index(body[matchedUntil:], prefix)
		if next < 0 {
			break
		}

		// Copy everything up to the match and move to the match
		copy(result[appendedUntil:], body[matchedUntil:matchedUntil+next])
		appendedUntil += next
		matchedUntil += next

		// A property is found, the next four bytes must be the prefix and big-endian length. Then
		keyLength := binary.BigEndian.Uint16(body[matchedUntil+1 : matchedUntil+3])

		// copy the type (1 byte), the length (2 bytes), and the key (length bytes) to the result, and advance
		toCopy := 3 + int(keyLength)
		copy(result[appendedUntil:], body[matchedUntil:matchedUntil+toCopy])
		appendedUntil += toCopy
		matchedUntil += toCopy

		// A key is copied, for which the value always must be application/pdf.
		copy(result[appendedUntil:], pdfMime)
		appendedUntil += len(pdfMime)

		// The first value always exists, read the length and skip
		matchedUntil += 2 + int(binary.BigEndian.Uint16(body[matchedUntil:matchedUntil+2]))

		// Another value starts with the type ("I") and a continuation character ("\u0000\u0000"). Keep reading until the next few characters are not continuation.
		for len(body) >= matchedUntil+5 && bytes.Equal(body[matchedUntil:matchedUntil+3], []byte("I\u0000\u0000")) {
			matchedUntil += 5 + int(binary.BigEndian.Uint16(body[matchedUntil+3:matchedUntil+5]))
		}
	}

	// Copy the remaining bytes
	copy(result[appendedUntil:], body[matchedUntil:])

	// Re-slice the byte slice to the correct length.
	return result[:appendedUntil+len(body)-matchedUntil]
}

func extractInt(name string, body []byte) (value int32, found bool) {
	bts := make([]byte, 3, len(name)+3)
	bts[0] = '!'
	binary.BigEndian.PutUint16(bts[1:], uint16(len(name)))
	bts = append(bts, []byte(name)...)

	index := bytes.Index(body, bts)
	if index < 0 {
		return
	}

	found = true

	// Read the next byte, this is the length
	start := index + len(bts)
	length := int(binary.BigEndian.Uint16(body[start:]))
	// Length should be smaller than 4 bytes
	if length > 4 || length <= 0 {
		// Give warning
		panic("oops")
	}

	value = int32(binary.BigEndian.Uint32(body[start+2:]))

	return
}

func dataOffset(body []byte) (offset int, err error) {
	defer func() {
		ei := recover()
		if ei == nil {
			return
		}

		switch e := ei.(type) {
		case error:
			err = e
			return
		default:
			err = fmt.Errorf("error encountered; %v", e)
		}
	}()

	/*
	  -----------------------------------------------
	  |                  version-number             |   2 bytes  - required
	  -----------------------------------------------
	  |               operation-id (request)        |
	  |                      or                     |   2 bytes  - required
	  |               status-code (response)        |
	  -----------------------------------------------
	  |                   request-id                |   4 bytes  - required
	  -----------------------------------------------------------
	  |               xxx-attributes-tag            |   1 byte  |
	  -----------------------------------------------           |-0 or more
	  |             xxx-attribute-sequence          |   n bytes |
	  -----------------------------------------------------------
	  |              end-of-attributes-tag          |   1 byte   - required
	  -----------------------------------------------
	  |                     data                    |   q bytes  - optional
	  -----------------------------------------------
	*/

	// headerSize := 8
	// body = body[headerSize:]

	offset = 8

	// var its, length int
	// var key, value string
	// 0x03 (ETX) marks the end of data
	var length int
	for len(body) > offset && body[offset] != 0x03 {
		offset++
		for len(body) > offset && body[offset] != 0x02 && body[offset] != 0x03 {
			offset++

			// Read the key
			length = int(binary.BigEndian.Uint16(body[offset:]))
			offset += 2 + length

			// Read the value
			length = int(binary.BigEndian.Uint16(body[offset:]))
			offset += 2 + length
		}
	}

	if len(body) > 0 {
		offset++
	}

	return
}

func cupsConvert(log zerolog.Logger, data []byte, mime, ppd string) (converted []byte, err error) {
	if cupsfilter == "" {
		return data, fmt.Errorf("cupsfilter must be set to convert to pdf")
	}

	// Convert to `mime`
	td := os.TempDir()
	var temp *os.File
	temp, err = os.CreateTemp(td, "cuproxy-preconvert-*")
	log.Trace().Err(err).Msg("created temp-file")
	if err != nil {
		return
	}

	// Close and remove temp when done
	defer os.Remove(temp.Name())
	defer temp.Close()

	n, err := temp.Write(data)
	log.Trace().Err(err).Int("num_bytes", n).Msg("written to temp-file")
	if err != nil {
		return
	}

	cmdRaw := []string{cupsfilter, temp.Name(), "-m", mime, "-P", ppd}

	cmd := exec.Command(cmdRaw[0], cmdRaw[1:]...)
	var b bytes.Buffer
	cmd.Stdout = &b
	cmd.Stderr = nil
	err = cmd.Run()
	converted = b.Bytes()

	log.Trace().Err(err).Strs("command", cmdRaw).Str("file", temp.Name()).Msg("converting")
	if err != nil {
		return
	}

	return
}

// extractPJL extracts, and returns, the PJL prefix and suffix, and actual
// 'to-be-printed' body of a PJL print job.
func extractPJL(contents []byte) (prefix []byte, suffix []byte, body []byte, err error) {
	body = contents

	// PJL body starts after "@PJL ENTER LANGUAGE = .*?\r\n" and ends with "@PJL EOJ"
	start := bytes.Index(body, []byte("@PJL ENTER LANGUAGE = "))
	if start < 0 {
		// `contents` does not contain a PJL job.
		return
	}

	end := bytes.LastIndex(body, []byte("@PJL EOJ"))
	if end < 0 {
		// Should not be possible! End of PJL must be found.
		err = fmt.Errorf("PJL job, but no end can be found")
		return
	}

	start += bytes.Index(body[start:], []byte("\n"))

	prefix = make([]byte, start)
	copy(prefix, body[:start])

	suffix = make([]byte, len(body)-end)
	copy(suffix, body[end:])

	// Re-slice, to get the in-fixed body
	body = body[start:end]

	return
}

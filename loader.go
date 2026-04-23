package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"

	"github.com/chebyrash/promise"
	"github.com/panjf2000/ants/v2"
	"github.com/rs/zerolog"
	"github.com/tuupke/utils/env"
	"github.com/tuupke/utils/lifecycle"
	"github.com/valyala/fasthttp"
)

var (
	cpuPool, ioPool promise.Pool
	printKeys       = strings.Split(env.String("PRINT_KEYS", "*"), ",")
)

func init() {
	cpuAnts, err := ants.NewPool(runtime.NumCPU())
	if err != nil {
		panic(err)
	}
	ioAnts, err := ants.NewPool(runtime.NumCPU() * 5)
	if err != nil {
		panic(err)
	}
	cpuPool = promise.FromAntsPool(cpuAnts)
	ioPool = promise.FromAntsPool(ioAnts)
}

// loadValues dispatches every configured webhook set for this request and
// returns a promise that resolves to the rendered (or cached) banner PDF.
//
// The promise always waits for every webhook to finish before rendering. A
// print request that arrives while webhooks are still in flight will block on
// Await rather than short-circuiting with partial data.
func loadValues(log zerolog.Logger, ctx *fasthttp.RequestCtx, jobId int32) *promise.Promise[*os.File] {
	data := LoadFromRequest(ctx)
	log = log.With().IPAddr("for", data.ip).Int32("job-id", jobId).Logger()
	log.Info().Int("num_hooks", len(toCall)).Msg("loading data")

	var wg sync.WaitGroup
	for _, set := range toCall {
		wg.Add(1)
		ioPool.Go(func() {
			defer wg.Done()
			set.run(log, data)
		})
	}

	allHooksDone := promise.New(func(resolve func(e), _ func(error)) {
		wg.Wait()
		log.Info().Msg("all webhooks finished")
		resolve(empty)
	})

	return promise.ThenWithPool(allHooksDone, lifecycle.Context(), func(_ e) (*os.File, error) {
		return renderBanner(log, data)
	}, cpuPool)
}

// renderBanner opens the per-IP banner PDF, reuses it if it's newer than the
// most recent webhook data, otherwise renders a fresh page.
func renderBanner(log zerolog.Logger, data *Props) (*os.File, error) {
	fn := pdfLocation + "/" + data.ip.String() + ".pdf"
	file, err := os.OpenFile(fn, os.O_RDWR|os.O_CREATE, 0o755)
	if err != nil {
		return nil, fmt.Errorf("open banner file %q: %w", fn, err)
	}

	if fi, statErr := file.Stat(); statErr == nil && fi != nil && fi.Size() > 0 &&
		!data.latestData.IsZero() && fi.ModTime().After(data.latestData) {
		log.Info().Msg("reusing cached banner")
		return file, nil
	}

	log.Err(file.Truncate(0)).Msg("rendering new banner")
	return file, BannerPage(log, file, data, printKeys...)
}

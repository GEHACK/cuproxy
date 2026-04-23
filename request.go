// Package web implements a web retrieval and caching mechanism
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/puzpuzpuz/xsync"
	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"
	"github.com/tuupke/utils/env"
	"github.com/tuupke/utils/lifecycle"
)

type (
	// etagPair is the cached ETag/Date pair for a single URL. Sent back on
	// subsequent requests as If-None-Match / If-Modified-Since so a webhook
	// can answer 304 Not Modified.
	etagPair struct {
		Key, Date string
	}

	// endpoint is a single webhook the proxy is configured to call.
	endpoint struct {
		method string
		name   string
		url    string
	}

	// endpoints is a group of endpoints that run sequentially — each one is
	// given the chance to contribute data that later ones can template into
	// their URLs.
	endpoints []endpoint

	// endpointsSet is the whole configuration: each endpoints group runs
	// concurrently with the others.
	endpointsSet []endpoints
)

var (
	// etagCache maps webhook URL → last seen ETag/Date pair.
	etagCache = xsync.NewMapOf[etagPair]()

	// pixieNonce is a per-process random identifier sent in X-Pixie-Nonce on
	// every outbound webhook. Lets the upstream correlate requests to this
	// proxy instance. Can be pinned via WEBHOOK_REQUEST_NONCE.
	pixieNonce = func() string {
		nonce := env.String("WEBHOOK_REQUEST_NONCE", "")
		if nonce == "" {
			const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890"
			src := rand.New(rand.NewSource(time.Now().UnixNano()))

			b := make([]byte, 32)
			for i := range b {
				b[i] = alphabet[src.Intn(len(alphabet))]
			}
			nonce = string(b)
		}
		zlog.Info().Str("nonce", nonce).Msg("X-Pixie-Nonce value")
		return nonce
	}()

	// maxWebhookTime caps how long any single webhook call can take. The
	// limit covers connect + request + full body read (see webhookClient).
	maxWebhookTime = env.Duration("WEBHOOK_MAX_DURATION", time.Second*30)

	// webhookClient is the HTTP client used for every outbound webhook. Its
	// Timeout wraps the entire round-trip including reading the response
	// body — we deliberately do not use context.WithTimeout + defer cancel
	// in ep.call because the body is read by the caller (parseResponse)
	// after call has returned, so an eager cancel truncates it mid-read.
	webhookClient = &http.Client{Timeout: maxWebhookTime}
)

// httpCall issues the actual HTTP request, threading ETag state and returning
// the response body for the caller to parse.
//
// Returned `loaded` is true when the server returned a 2xx with a new body —
// false on a 304 Not Modified (no new data, not an error) or on non-2xx
// (returned along with err).
func httpCall(
	ctx context.Context,
	log zerolog.Logger,
	url, verb string,
	ip net.IP,
	requestBody io.Reader,
) (responseBody io.ReadCloser, responseType string, loaded bool, err error) {
	req, err := http.NewRequestWithContext(ctx, verb, url, requestBody)
	if err != nil {
		err = fmt.Errorf("cannot create request [%v] '%v'", verb, url)
		return
	}

	req.Header.Set("User-Agent", "Pixie/CupsProxy")
	req.Header.Set("Accept", "application/json, image/*")
	req.Header.Set("X-Pixie-Nonce", pixieNonce)
	if ip != nil {
		req.Header.Set("X-Forwarded-For", ip.String())
	}

	if etag, ok := etagCache.Load(url); ok {
		req.Header.Add("If-None-Match", etag.Key)
		req.Header.Add("If-Modified-Since", etag.Date)
	}

	resp, err := webhookClient.Do(req)
	var statusCode int
	if resp != nil {
		statusCode = resp.StatusCode
	}

	log.Err(err).Int("status", statusCode).Msg("called hook")
	if err != nil {
		err = fmt.Errorf("cannot create request [%v] '%v'", verb, url)
		return
	}

	if resp.StatusCode == http.StatusNotModified {
		err = resp.Body.Close()
		log.Err(err).Msg("status not changed, continuing")
		return
	}

	responseBody, responseType, loaded = resp.Body, resp.Header.Get("content-type"), true

	if key, date := resp.Header.Get("ETag"), resp.Header.Get("Date"); key != "" || date != "" {
		etagCache.Store(url, etagPair{Key: key, Date: date})
	}

	if resp.StatusCode/100 != 2 {
		log.Warn().Msg("status not successful, not continuing to next hook")
		err = fmt.Errorf("non-successful status code received (%v)", resp.StatusCode)
	}

	return
}

// call expands the endpoint's URL template, builds a request body for non-GET
// methods, and dispatches the HTTP call. Each call is bounded by
// WEBHOOK_MAX_DURATION.
func (ep endpoint) call(
	ctx context.Context,
	log zerolog.Logger,
	data *Props,
) (io.ReadCloser, string, bool, error) {
	u, params := replaceParameters(ep.url, data, ep.name)
	log.Debug().
		Str("original", ep.url).
		Object("relevant-data", params).
		Str("result", u).
		Msg("replaced url")

	var reqBody io.Reader
	if ep.method != http.MethodGet {
		reqBody = data.json(map[string]string{
			"webhook_name":   ep.name,
			"webhook_method": ep.method,
			"webhook_url":    u,
		})
	}

	return httpCall(ctx, log, u, ep.method, data.ip, reqBody)
}

// parseResponse inspects the webhook response and updates `data` accordingly.
//
// Images (image/jpeg, image/png, image/gif) are saved to disk and their path
// is stored under the endpoint's name. JSON responses have their top-level
// keys stored as string values; keys listed in IMAGE_KEYS are instead
// returned as follow-up endpoints so the URL can be fetched as an image.
func (ep endpoint) parseResponse(
	log zerolog.Logger,
	respBody io.ReadCloser,
	respType string,
	data *Props,
) (nested endpoints) {
	respType = strings.Split(respType, ";")[0]
	log.Info().Str("response_type", respType).Msg("handling response")

	switch respType {
	case "image/jpeg", "image/png", "image/gif":
		fn := downloadTo + "/" + data.ip.String() + "_" + ep.name + "." + respType[6:]
		f, err := os.OpenFile(fn, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o755)
		if err != nil {
			log.Warn().Err(err).Str("filename", fn).Msg("could not open imagefile")
			return nil
		}
		defer f.Close()

		n, err := io.Copy(f, respBody)
		log.Debug().Err(err).Int64("num-bytes", n).Str("filename", fn).Msg("written image")
		if err != nil {
			log.Warn().
				Str("filename", fn).
				Err(err).
				Msg("image writing failed, not storing the path")
			return nil
		}

		data.Store(ep.name, fn)

	case "application/json":
		var jsonData map[string]any
		if err := json.NewDecoder(respBody).Decode(&jsonData); err != nil {
			log.Info().Msg(fmt.Sprintf("%+v", jsonData))
			log.Err(err).Msg("could not decode json")
			return nil
		}

		for k, v := range jsonData {
			str, ok := anyToString(v)
			if !ok {
				continue
			}

			// Keys listed in IMAGE_KEYS hold a URL that we need to fetch as
			// an image: queue it up as a nested endpoint.
			if slices.Contains(imageKeys, k) {
				if _, err := url.Parse(str); err != nil {
					log.Debug().
						Err(err).
						Str("url", str).
						Str("key", k).
						Msg("image-key value is not a valid url, skipping")
					continue
				}

				log.Debug().Str("url", str).Str("key", k).Msg("queued nested image fetch")
				nested = append(nested, endpoint{
					method: http.MethodGet,
					name:   k,
					url:    str,
				})
				continue
			}

			// Regular value: optionally rewrite the key via WEBHOOK_KEY_TEMPLATE.
			if keyTemplate != "" {
				data.Store("webhook_key", k)
				orig := k
				var params mapWriter
				k, params = replaceParameters(keyTemplate, data, ep.name)
				log.Debug().
					Str("original", orig).
					Str("template", keyTemplate).
					Object("relevant-data", params).
					Str("result", k).
					Msg("filled template for key")
			}

			data.Store(k, str)
		}

	default:
		log.Warn().
			Str("content-type", respType).
			Msg("unsupported content type encountered, ignored")
	}

	return nested
}

// run executes every endpoint in the set sequentially, then fans out any
// nested image endpoints discovered in the JSON responses in parallel.
//
// Sequential execution of the primary set is deliberate: later endpoints can
// template values set by earlier ones into their URLs.
func (eps endpoints) run(log zerolog.Logger, data *Props) {
	nested, ok := eps.runSequential(log, data)
	if !ok || len(nested) == 0 {
		return
	}
	nested.runParallel(log, data)
	data.latestData = time.Now()
}

// runSequential runs each endpoint in order. Returns the nested endpoints
// discovered along the way and whether the whole sequence completed without
// an error (on error we abort the whole group to match previous behaviour).
func (eps endpoints) runSequential(log zerolog.Logger, data *Props) (endpoints, bool) {
	nested := make(endpoints, 0)
	for _, ep := range eps {
		epLog := log.With().
			Str("verb", ep.method).
			Str("url", ep.url).
			Bool("with-ip", data.ip != nil).
			Logger()

		body, contentType, loaded, err := ep.call(lifecycle.Context(), epLog, data)
		epLog.Err(err).Bool("new-data", loaded).Msg("request executed")
		if err != nil {
			return nil, false
		}
		if !loaded {
			epLog.Debug().Msg("no new data loaded, skipping response handling")
			continue
		}

		data.latestData = time.Now()
		nested = append(nested, ep.parseResponse(epLog, body, contentType, data)...)
	}
	return nested, true
}

// runParallel dispatches every endpoint concurrently. Used for nested image
// fetches where order does not matter and each one writes to a distinct key.
func (eps endpoints) runParallel(log zerolog.Logger, data *Props) {
	var wg sync.WaitGroup
	for _, ep := range eps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			epLog := log.With().
				Str("verb", ep.method).
				Str("url", ep.url).
				Bool("nested", true).
				Logger()

			body, contentType, loaded, err := ep.call(lifecycle.Context(), epLog, data)
			epLog.Err(err).Bool("new-data", loaded).Msg("nested request executed")
			if err != nil || !loaded {
				return
			}
			ep.parseResponse(epLog, body, contentType, data)
		}()
	}
	wg.Wait()
}

// anyToString stringifies a JSON-decoded value if it is one of the scalar
// types we know how to render. ok=false on maps, slices of unknown type, nil,
// etc. — those are dropped rather than stored.
func anyToString(vi any) (strVal string, ok bool) {
	ok = true
	switch v := vi.(type) {
	case string:
		strVal = v
	case int:
		strVal = strconv.Itoa(v)
	case uint8:
		strVal = strconv.Itoa(int(v))
	case int8:
		strVal = strconv.Itoa(int(v))
	case uint16:
		strVal = strconv.Itoa(int(v))
	case int16:
		strVal = strconv.Itoa(int(v))
	case uint32:
		strVal = strconv.Itoa(int(v))
	case int32:
		strVal = strconv.Itoa(int(v))
	case uint64:
		strVal = strconv.FormatUint(v, 10)
	case int64:
		strVal = strconv.FormatInt(v, 10)
	case bool:
		strVal = strconv.FormatBool(v)
	case float64:
		strVal = strconv.FormatFloat(v, 'f', 10, 64)
	case float32:
		strVal = strconv.FormatFloat(float64(v), 'f', 10, 32)
	case []string:
		strVal = strings.Join(v, ", ")
	default:
		ok = false
	}

	return
}

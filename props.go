package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/puzpuzpuz/xsync"
	"github.com/rs/zerolog"
	"github.com/tuupke/utils/env"
	"github.com/valyala/fasthttp"
	"github.com/valyala/fasttemplate"
)

// Props holds the per-request bag of key/value data we expose to webhook URLs,
// webhook bodies, and the banner template. It is keyed by IP + basic-auth +
// request URI (see Load) and survives across requests so repeat prints from
// the same client reuse accumulated data.
type Props struct {
	ip net.IP

	*xsync.MapOf[string, string]

	// latestData is the time of the most recent successful webhook response.
	// Used by the banner cache to decide whether to re-render.
	latestData time.Time
}

// e is a zero-sized token used where Go's type system wants a value but we
// only care about the signal. `empty` is the canonical instance.
type e struct{}

var empty = e{}

var (
	props = xsync.NewMapOf[Props]()

	includeBasicAuth = env.Bool("BASIC_AUTH_IN_DATA", false)
	basicAuthUser    = env.String("BASIC_AUTH_USERNAME", "ba_username")
	basicAuthPass    = env.String("BASIC_AUTH_PASSWORD", "ba_password")

	imageKeys   = strings.Split(env.String("IMAGE_KEYS", ""), ",")
	keyTemplate = env.String("WEBHOOK_KEY_TEMPLATE", "")
	downloadTo  = env.String("WEBHOOK_TEMP_DIR", os.TempDir())

	toCallString = env.String("WEBHOOKS_TO_CALL", "")
	toCall       endpointsSet
)

// methods is the set of HTTP verbs accepted in WEBHOOKS_TO_CALL.
var methods = map[string]struct{}{
	http.MethodGet:     empty,
	http.MethodHead:    empty,
	http.MethodPost:    empty,
	http.MethodPut:     empty,
	http.MethodPatch:   empty,
	http.MethodDelete:  empty,
	http.MethodConnect: empty,
	http.MethodOptions: empty,
	http.MethodTrace:   empty,
}

func init() {
	if len(toCallString) > 0 {
		var err error
		toCall, err = parseToCallString(toCallString)
		if err != nil {
			panic(err)
		}
	}

	if downloadTo == "" {
		downloadTo = os.TempDir()
		fmt.Println("Empty download dir, using", downloadTo)
	}

	if err := os.MkdirAll(downloadTo, 0o755); err != nil {
		panic(fmt.Errorf("could not create download folder '%v'; %w", downloadTo, err))
	}
}

// parseToCallString parses the WEBHOOKS_TO_CALL env var into an endpointsSet.
// Format: "name;METHOD;url|name;METHOD;url && name;METHOD;url" — sets separated
// by "&&", endpoints within a set separated by "|". Each set runs sequentially,
// each set is dispatched concurrently with the others.
func parseToCallString(toCallString string) (e endpointsSet, gerr error) {
	eps := strings.Split(toCallString, "&&")
	e = make([]endpoints, len(eps))
	for k, set := range eps {
		ep := strings.Split(set, "|")
		e[k] = make([]endpoint, len(ep))
		for kk, end := range ep {
			s := strings.SplitN(end, ";", 3)
			if len(s) < 3 {
				gerr = fmt.Errorf("invalid webhook spec found, expected 3 parts: '%v'", e)
				return
			}

			if _, err := url.Parse(s[2]); err != nil {
				gerr = fmt.Errorf("could not parse url '%v'; %w", s[2], err)
				return
			}

			if _, ok := methods[s[1]]; !ok {
				gerr = fmt.Errorf(
					"could not parse method '%v' for url '%v', expected values look like GET, POST, DELETE",
					s[1],
					s[2],
				)
				return
			}

			e[k][kk] = endpoint{
				name:   s[0],
				method: s[1],
				url:    s[2],
			}
		}
	}

	return
}

// mapWriter is a map[string]string that knows how to serialize itself into a
// zerolog event. Used for logging the template parameters that were actually
// substituted into a webhook URL.
type mapWriter map[string]string

func (m mapWriter) MarshalZerologObject(e *zerolog.Event) {
	for k, v := range m {
		e.Str(k, v)
	}
}

// replaceParameters expands `{{key}}` placeholders in `template` using values
// from data. The `webhook_name` key is special-cased to the current webhook's
// name. Returns the expanded string and a mapWriter of the values actually
// consumed (for logging).
func replaceParameters(template string, data *Props, webhookname string) (string, mapWriter) {
	d := make(mapWriter)
	return fasttemplate.New(template, "{{", "}}").
			ExecuteFuncString(func(w io.Writer, tag string) (int, error) {
				if tag == "webhook_name" {
					d[tag] = webhookname
					return w.Write([]byte(webhookname))
				}

				v, _ := data.Load(tag)
				d[tag] = v
				return w.Write([]byte(v))
			}),
		d
}

// decodeBasicAuth parses an HTTP `Authorization: Basic ...` header into its
// user/pass components. ok=false if the header is missing, not basic auth, or
// malformed.
func decodeBasicAuth(auth []byte) (ok bool, user, pass string) {
	before, after, hasSpace := bytes.Cut(auth, []byte{' '})
	if !hasSpace || !bytes.EqualFold(before, []byte("basic")) {
		return
	}

	decoded, err := base64.StdEncoding.DecodeString(string(after))
	if err != nil {
		return
	}

	credentials := bytes.Split(decoded, []byte(":"))
	if len(credentials) <= 1 {
		return
	}

	user, pass, ok = string(credentials[0]), string(credentials[1]), true
	return
}

// LoadFromRequest extracts the lookup key (IP + basic-auth + URI) from an
// incoming request, seeds the initial Props data from path segments of the
// form `key=value`, and returns the (possibly pre-existing) Props for that key.
func LoadFromRequest(ctx *fasthttp.RequestCtx) *Props {
	ok, user, pass := decodeBasicAuth(ctx.Request.Header.Peek("Authorization"))
	ip := ctx.RemoteIP()

	baseData := make(map[string]string)
	for segment := range strings.SplitSeq(strings.Trim(string(ctx.Request.URI().Path()), "/"), "/") {
		keyValues := strings.SplitN(segment, "=", 2)
		if len(keyValues) > 1 {
			baseData[keyValues[0]] = keyValues[1]
		}
	}

	if ok && includeBasicAuth {
		baseData[basicAuthUser] = user
		baseData[basicAuthPass] = pass
	}

	baseData["requesting_ip"] = ip.String()
	return Load(ip, baseData, user, pass, ctx.Request.URI().String())
}

// Load returns the Props for a given (ip, segments...) key, creating one
// seeded with baseData if it does not already exist.
func Load(ip net.IP, baseData map[string]string, segments ...string) *Props {
	key := ip.String() + "::" + strings.Join(segments, "::")
	props, loaded := (*xsync.MapOf[string, *Props])(props).LoadOrStore(key, &Props{
		ip:    ip,
		MapOf: xsync.NewMapOf[string](),
	})

	if !loaded {
		for k, v := range baseData {
			props.Store(k, v)
		}
	}

	return props
}

// Reduce returns a copy of the Props restricted to the given keys.
func (p *Props) Reduce(keys ...string) (mp map[string]string) {
	mp = make(map[string]string)
	for _, key := range keys {
		if data, ok := p.Load(key); ok {
			mp[key] = data
		}
	}

	return
}

// json serializes the Props (plus the extra map) into a flat JSON object.
// Used as the body for non-GET webhook calls.
func (p *Props) json(extra map[string]string) io.Reader {
	// TODO create a pool of buffers to use
	b := new(bytes.Buffer)
	b.WriteByte('{')

	var notFirst bool
	write := func(key, value string) bool {
		if notFirst {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		b.WriteString(strings.ReplaceAll(key, `"`, `\"`))
		b.WriteByte('"')
		b.WriteString(":")

		b.WriteByte('"')
		b.WriteString(strings.ReplaceAll(value, `"`, `\"`))
		b.WriteByte('"')

		notFirst = true
		return notFirst
	}

	p.Range(write)

	for k, v := range extra {
		_ = write(k, v)
	}

	b.WriteByte('}')

	return b
}

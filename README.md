# CUProxy, a simple ipp 1.0-2.2 compliant banner proxy

This project was started to solve a problem faced within ICPC-style programming contests: "How to determine which team 
submitted which printed pages?"

In these programming contests it is customary for teams to be able to print their sourcecode so they can continue
debugging while a teammate has the use of the (single) team machine. These prints must be private to the teams
that issued the prints to prevent cheating. In practice, the only way of ensuring this is to have the organisation
handing out the print-jobs instead of teams retrieving the jobs themselves.

The main problem with this is that the printer operator needs to know which pages belong to which team. In
linux-based contest environments the printing services are commonly setup using CUPS. CUPS has built-in support for
banner pages printing most - if not all - of the required information when setup properly. Though a program can override
this behavior which would result in a 'mystery-print' without a banner page.

CUProxy can be used to prevent this issue. As a small IPP proxy that listens for print-jobs, converts the printed file when needed, constructs and retrieves data, creates a banner-page, and appends (or prepends) the banner to the document. The only requirement is that the actual printer printer supports printing PDF documents.

When CUProxy sees a printing request, it will - in parallel to proxy-ing the request - start by constructing the banner
page. There are two ways of getting data on the banner-pages. The first is by telling CUProxy the data using key-value
pairs in the printer-url. The second - and much more powerful - method is to use (JSON returning) webhooks. These data are
then 'printed' on a (single) banner page. CUProxy has basic support for printing an image on the banner.
Webhook, and KV-url configuration are explained in the configuration section.

# Installation
Binaries built from go are highly portable, due to them being statically compile(able). The only true dependency of CUProxy
is `cupsfilters` which it uses to convert non-PDF prints into PDF. If you are certain that all programs used to print
support PDF configuring `cupsfilters` can be omitted.

To start dockerized with conversion to PDF *enabled*, ensure the `CUPSFILTER_LOCATION` points to wherever `cupsfilter` is located on your system. e.g.:
 - `CUPSFILTER_LOCATION=$(which cupsfilter) ./cuproxy-linux-amd64`
 - `CUPSFILTER_LOCATION=/usr/bin/cupsfilter ./cuproxy-linux-amd64`

The easiest is to run CUProxy using docker since it comes preconfigured with cupsfilter.

### Docker
Simply run the `tuupke/cuproxy` image:

```bash
# Attached, so the logs can be seen
docker run -e "PRINTER_TO=print_server:631/printers/Actual_Printer" --net host --rm -it tuupke/cuproxy

# Detached for 'normal' operations
docker run -e "PRINTER_TO=print_server:631/printers/Actual_Printer" --net host -d --name CUProxy tuupke/cuproxy
```

Note, since CUProxy has to replace parts of the IPP requests, and responses it is *required* that it does not receive requests from the docker-proxy with a docker-internal IP address. The listed examples do not have this issue since they run in host networking mode.

### Binary

1. Download the latest version from the releases page. Ensuring you download the binary for the correct OS and architecture. The following OS and architecture pairs are currently available:
   - Linux/amd64
   - Linux/386
   - Linux/amd
   - Linux/amd64
   - Darwin/amd64
   - Darwin/arm64
2. Copy the binary to the desired location.
3. Optionally, create a `.env` file containing the configuration in the same folder as where the binary is located.
4. Run the binary: `./cuproxy`. Note, running CUProxy as root is required when using the default configured port of `631`. Configure a port above `1024` to run CUProxy using a non-root user.

#### systemd service
The following systemd service can be used to ensure the proxy is running and have it automatically restarted after a crash.

1. Store the file contents listed below in a file called `/etc/systemd/system/multi-user.target.wants/cuproxy.service`
2. Reload the configured services `sudo systemctl daemon-reload`.
3. Start CUProxy `sudo systemctl start cuproxy.service`.

```unit file (systemd)
[Unit]
Description=CUProxy
After=network.target
StartLimitIntervalSec=0

[Service]
Type=simple
Restart=always
RestartSec=1
User=root

# Point to the CUProxy binary
ExecStart=/opt/pixie/cuproxy-linux-amd64

# Add the config here, or store the config in `/opt/pixie/.env` 
Environment=PRINTER_TO=....
# Environment=...
# ...

[Install]
WantedBy=multi-user.target
```

### `go get`
If you have the go toolchain installed locally, you can install CUProxy using `go get`.

To install execute `go install github.com/tuupke/cuproxy@latest`.

To run execute `$(go env GOPATH)/bin/cuproxy`.

### From source
Installing from source is useful if you want to develop on CUProxy.

```bash
# Clone the repo
git clone github.com/tuupke/pixie

# Change directory
cd pixie/cuproxy

# Ensure all dependencies are available
go mod download

# Run CUProxy
go run .
```

## Running CUProxy
Running CUProxy consists of 2 parts. (1) running CUProxy and (2) configuring a client.

In order to run CUProxy you need to point it to an IPP printer. Since CUPS is a valid IPP printer you can point CUProxy to a preconfigured CUPS queue. Printjobs received by CUProxy will then be proxied to CUPS which will then do whatever is needed to print.

Since CUProxy implements a (mostly transparent) IPP proxy you can simply configure the client CUPS instance to 'print'
to CUProxy instead of directly to a printer, or to the main CUPS server.
Select "Generic PDF Printer (en)" or "IPP Everywhere™" as the driver when required.

# Configuration
CUProxy is configured using the environment. 
For convenience all environment settings can be stored in a file called `.env`, 
with each line of the file containing one environment `KEY=VALUE` pair, and stored in the same directory as the binary.

To make the descriptions easier a distinction between environment-data and banner-data has to be made.
Environment-data is all data that is stored on the environment.
Banner-data is all data that might be printed on a banner page.

Banner-data is cached between print-jobs and gets refreshed once a new job starts. 
If the data is not fresh when the to-be-printed file is sent previously requested data is used unless `BANNER_DATA_ALWAYS_FRESH` is set to true.
Banner-data is always seeded with the basic-auth username, and password if present and required, and the IP address where the request originated. Stored in the banner-data key `requesting_ip`.

The URL where the print request got issued is also parsed and stored in the banner-data. Every path-segment is assumed to contain a key-value pair similar to the environment. 
A print-job originating from 10.13.37.12, and printed to `ipp://ppp:6631/foo=fooz/bar=foobar` will result in the (pre-webhook) banner-data of:
 - `originating_ip`: "10.13.37.12"
 - `foo`: "fooz"
 - `bar`: "foobar"

## Global settings
These are the 'generic' settings for CUProxy. 

| Variable            | Type      | Default            | Description                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |
|---------------------|-----------|--------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `LOG_LEVEL`         | `String`  | "info"             | The level of verbosity for the log-items generated. Possible values are: `panic`, `fatal`, `error`, `warn`, `info`, `debug`, `trace`, and `disabled`.                                                                                                                                                                                                                                                                                                                                                  |
| `PRINTER_TO`        | `String`  | ""                 | The IPP url of where the actual printer is located. This format is quite exact. No protocol should be added, but the port should always be present! For example: `localhost:631/printers/Virtual_PDF_Printer`                                                                                                                                                                                                                                                                                          |
| `PRINTER_TLS`       | `Boolean` | false              | Whether the actual printer is an `ipps` queue. When `true`, jobs are forwarded over HTTPS and the printer-uri is rewritten to `ipps://<PRINTER_TO>`. Certificates are verified against the system trust store, a corporate CA can be added through `SSL_CERT_FILE` or `SSL_CERT_DIR`.                                                                                                                                                                                                                      |
| `PRINTER_TLS_SKIP_VERIFY` | `Boolean` | false        | Skip verification of the printer's TLS certificate. Only use this for printers with self-signed certificates on a trusted network.                                                                                                                                                                                                                                                                                                                                                                    |
| `PRINTER_USERNAME`  | `String`  | ""                 | When set, the `requesting-user-name` of every proxied IPP request is replaced by this value, so all jobs are owned by the same user on the actual printer. Leave empty to forward the submitter's username unchanged.                                                                                                                                                                                                                                                                                  |
| `LISTEN`            | `String`  | ":631"             | IP + port where to listen on. Defaults to `0.0.0.0:631` which conflicts with CUPS when installed on the same machine.                                                                                                                                                                                                                                                                                                                                                                                  |
| `DUMP_IPP_CONTENTS` | `String`  | ""                 | The location on disk where to store proxied IPP messages. Leave empty to disable. Does nothing when `DUMP_ORIGINAL` and `DUMP_REPLACEMENTS` are both `false`. The dumped files have the following filenames: `<seq-id>-<dir>-<type>.bin` where `seq-id` is an incrementing integer uniquely identifying the request; `dir` the "direction", is it the request ("req"), or is it the printers response (res); and `type` depicts whether it is the original ("orig"), or the modified request ("repl"). |
| `DUMP_ORIGINAL`     | `Boolean` | false              | Whether to dump the original contents. Does nothing when `DUMP_IPP_CONTENTS` is empty.                                                                                                                                                                                                                                                                                                                                                                                                                 |
| `DUMP_REPLACEMENTS` | `Boolean` | false              | Whether to dump the replaced contents. Does nothing when `DUMP_IPP_CONTENTS` is empty.                                                                                                                                                                                                                                                                                                                                                                                                                 |
| `MAX_REQUEST_SIZE`  | `Integer` | 134217728 (128MiB) | The max request size that CUProxy will accept. This automatically limits the maximum file-size of the to-be-printed document. This value does not exclude the 'ipp overhead', which is commonly about 1 to 2 KiB.                                                                                                                                                                                                                                                                                      |

### Banner related settings
These are the configuration variables related to the banner page.

| Variable                   | Type      | Default       | Description                                                                                                                                                                                                                                                                                                     |
|----------------------------|-----------|---------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `BANNER_APPEND`            | `Boolean` | false         | Whether to append the banner instead of prepending it to the print job.                                                                                                                                                                                                                                         |
| `BANNER_ON_BACK`           | `Boolean` | false         | Whether to put the banner on the back of a page. Assumes - but does not check whether for - a duplexer!                                                                                                                                                                                                         |
| `PRINT_KEYS`               | `String`  | "*"           | Comma separated list of keys from the banner-data that must be printed on the banner page. The special value "*" prints all keys in alphabetical order. When the keys are specified separately they will be printed in the same order that they were defined here. If a key is unknown, it will not be printed. |
| `BANNER_MUST_EXIST`        | `Boolean` | false         | Whether to panic when no banner can be rendered. Prevents the job from being printed without a banner page.                                                                                                                                                                                                     |
| `BANNER_DATA_ALWAYS_FRESH` | `Boolean` | false         | Whether to always wait for fresh data, or whether using previous retrieved data is also fine. Useful for when data retrieved using webhooks does not change.                                                                                                                                                    |
| `BASIC_AUTH_IN_DATA`       | `Boolean` | false         | Set this variable to true to include the basic-auth username and password used to connect to CUProxy (if any) in the banner-data. The basic-auth feature is not thoroughly and should also not be relied upon for security since CUProxy does not (yet) support encryption.                                     |
| `BASIC_AUTH_USERNAME`      | `String`  | "ba_password" | If `BASIC_AUTH_IN_DATA` is set to true, the key where the basic-auth username will be stored in the banner-data.                                                                                                                                                                                                |
| `BASIC_AUTH_PASSWORD`      | `String`  | "ba_username" | If `BASIC_AUTH_IN_DATA` is set to true, the key where the basic-auth password will be stored in the banner-data.                                                                                                                                                                                                |
| `IMAGE_KEY`                | `String`  | ""            | The name of the key in the banner-data pointing to a valid image to be rendered. When CUProxy encounters the `IMAGE_KEY` it attempts to (down)load the image and prints it on the banner page when included in `PRINT_KEYS`.                                                                                    |

### Webhook settings
Webhooks are the more powerful way of building banner-data. 
The implementation allows for the concurrent, or sequential, 
retrieval of data and can be used to access CLICCS contest-api compliant tools.

CUProxy assumes that data returned from a webhook is either an image, or valid JSON data. 
All other types are ignored.

Webhooks in CUProxy are named, can be either executed sequentially, or concurrently, and support parameters in their urls.
The name of a webhook can be used to ensure that the duplicated keys do not get overwritten. 
This can be achieved by setting the `WEBHOOK_KEY_TEMPLATE` variable to a template.
The banner-data is used as the data to render templates.
The name of the webhook is stored in the `webhook_name` key in the banner data. The key-name will be stored as `webhook_key` in the banner data.
The template languages uses "moustache syntax". All strings similar to "{{ key_name }}", will be replaced by the contents of `key_name` within the banner-data, or the empty string if the key does not exist. 
e.g. To depuplicate the results of webhooks named "user", and "team", set `WEBHOOK_KEY_TEMPLATE` to "{{ webhook_name }}_{{ webhook_key }}". 


The only reason to make request execute sequentially is if there is a dependency between the webhooks.
e.g.
The CLICCS-api allows for retrieving a team's display-name. 
To do this the , knowing the team's id is required.
Assuming the user is automatically logged in the `/user` endpoint returns the data for the (currently logged in) user, which includes the team-id
The team-id can then be used as a parameter for the `/teams` endpoint. This will be demonstrated in the listed example.

The webhooks allow for different http-verbs other than GET. 
All currently retrieved data will be serialized and sent along as JSON. 
Be aware when using a non-GET verb. 
Incorrect configuration can lead to a data leak.

Note, CUProxy ignores results from webhooks that do not return a 2xx status-code, or that don't exist.

### Webhooks example
The following configuration example shows how to configure and use the webhooks. 
It will call 3 groups of sequential of webhooks in parallel. Sequential webhooks are separated by "|".
Sequential groups that will be executed in parallel are separated by "&&".
Only 1 level of nesting is supported. `WEBHOOKS_TO_CALL` will first be split on "&&", then on |. 
Those groups of sequential webhooks will then be executed in parallel.

In this example, 2 groups of webhooks are executed in parallel. 
 - The first - named 'misc' - executes a POST request to a non-existing endpoint.
 - The second - name 'user' - retrieves the logged-in user's information. Then retrieves the team information using the team-id, and hardcoded (jury) credentials.
 - The last retrieves an image based on the ip address.

```
misc;POST;https://www.domjudge.org/api/non-existing-endpoint&&user;GET;https://www.domjudge.org/demoweb/api/user?strict=false|team;GET;https://jury:jury@www.domjudge.org/demoweb/api/teams/{{team_id}}?strict=false&&image;GET;http://pixie.local/{{originating_ip}}.jpg
```

In a more readable format:
 - Group 1
   - Request 1.1:
     - Name: "misc"
     - Verb: "POST"
     - url: "https://www.domjudge.org/api/non-existing-endpoin"
 - Group 2
   - Request 2.1:
      - Name: "user"
      - Verb: "GET"
      - url: "https://www.domjudge.org/demoweb/api/user?strict=false"
   - Request 2.2:
      - Name: "team"
      - Verb: "GET"
      - url: "https://jury:jury@www.domjudge.org/demoweb/api/teams/{{team_id}}?strict=false"
 - Group 3
   - Request 3.1:
      - Name: "image"
      - Verb: "GET"
      - url: "http://pixie.local/{{originating_ip}}.jpg"


| Variable                | Type      | Default       | Description                                                                                                                                                                                                                         |
|-------------------------|-----------|---------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `WEBHOOK_REQUEST_NONCE` | `String`  | ""            | A nonce which is added as `X-Pixie-Nonce` header when executing webhooks. Defaults to a randomized string of 32 characters. Can be used to authenticate CUProxy to the webhook server. This value will never be exposed to clients. |
| `WEBHOOK_TEMP_DIR`      | `String`  | "/tmp"        | Where images and other static resources will be cached once downloaded. Uses e-tags to determine whether a newer version should be included.                                                                                        |
| `WEBHOOK_KEY_TEMPLATE`  | `String`  | ""            | Whether to put the banner on the back of a page. Assumes but does not check whether a duplexer is installed!                                                                                                                        |
| `WEBHOOKS_TO_CALL`      | `String`  | ""            | Which webhooks to call, the format is specified below.                                                                                                                                                                              |
| `WEBHOOK_MAX_DURATION`  | Duration` | "30s"         | The maximum time the webhooks can execute. This is accounted separately for every sequential webhook set.                                                                                                                           | 

### PDF settings
`gofpdf` is used for rendering the banner page, while `pdfcpu` is used to prepend (or append) the bannerpage to the actual print. The following variables can be set.

| Variable          | Type      | Default | Description                                                                                                                                     |
|-------------------|-----------|---------|-------------------------------------------------------------------------------------------------------------------------------------------------|
| `PDF_LOCATION`    | `String`  | "/tmp"  | Where to store the rendered banner pages.                                                                                                       |
| `PDF_UNIT`        | `String`  | "mm"    | The unit to use for the other settings. Supported values are: millimeter `mm`, centimeter `cm`, point `pt`, inch `in`.                          |
| `PDF_PAGE_SIZE`   | `String`  | "A4"    | The size of the PDF to generate. Supported values are: `A3`, `A4`, `A5`, `Letter`, `Legal`, and `Tabloid`.                                      |
| `PDF_FONT_DIR`    | `String`  | "."     | The file system location in which font resources will be found. Currently unused as different fonts are not supported.                          |
| `PDF_FONT`        | `String`  | "Arial" | The name of the font that is in used on the banner page.                                                                                        |
| `PDF_FONT_SIZE`   | `Float`   | 12      | The size of the font that is used on the banner page.                                                                                           |
| `PDF_LANDSCAPE`   | `Boolean` | false   | Whether to render the banner in landscape.                                                                                                      |
| `PDF_LEFT_MARGIN` | `Float`   | 10      | The number of `PDF_UNIT` units to leave blank at the left of the banner. Note, no right margin can be set, long lines will not be split in two! |
| `PDF_TOP_MARGIN`  | `Float`   | 10      | The number of `PDF_UNIT` units to leave blank at the top of the banner.                                                                         |
| `PDF_LINE_HEIGHT` | `Float`   | 1.2     | The line-height of each line. A multiplier to font-size.                                                                                        |


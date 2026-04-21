FROM golang:1.21-alpine AS build

ADD . /src
WORKDIR /src

USER root

# Run the installer
RUN GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -mod=vendor -ldflags="-w -s" -o /cuproxy .

# Build a new image, copy everything over and set the entrypoint
FROM alpine:3

COPY --from=build /cuproxy /cuproxy

EXPOSE 631

# Include cups and cups-filters to convert to pdf.
RUN apk add --no-cache cups cups-filters typst

ENV PPD_LOCATION=/usr/share/ppd/cupsfilters/Generic-PDF_Printer-PDF.ppd
ENV CUPSFILTER_LOCATION=/usr/sbin/cupsfilter

CMD ["/cuproxy"]

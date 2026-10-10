FROM golang:1.26.9 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY schemas ./schemas
COPY plugins ./plugins
COPY api ./api
RUN CGO_ENABLED=0 go build -trimpath -o /out/lantai ./cmd/lantai

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/lantai /usr/local/bin/lantai
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/lantai"]
CMD ["serve", "--home", "/var/lib/lantai"]

# TopoLight — single static binary, standard library only.
# Build:  docker build -t topolight:0.1.0 .
# Run:    docker run -d -p 8432:8432 -p 514:514/udp -p 514:514 -p 162:162/udp -v topolight-data:/data topolight:0.1.0
#
# ISSUER_PUBKEY is intentionally empty: an image built from this public repo
# is the Free edition. Pro/Team customers receive a binary with the issuer
# key baked in; run that one with the same flags.
FROM golang:1.24-alpine AS build
ARG ISSUER_PUBKEY=""
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/nizartuanku/topolight/internal/license.IssuerPublicKey=${ISSUER_PUBKEY}" \
      -o /out/topolight ./cmd/topolight

FROM alpine:3.20
RUN adduser -D -h /data -u 10001 topolight && apk add --no-cache libcap ca-certificates \
 && mkdir -p /data && chown topolight:topolight /data
COPY --from=build /out/topolight /usr/local/bin/topolight
RUN setcap 'cap_net_raw,cap_net_bind_service=+ep' /usr/local/bin/topolight
USER topolight
VOLUME /data
EXPOSE 8432/tcp 514/udp 514/tcp 162/udp
ENTRYPOINT ["/usr/local/bin/topolight"]
CMD ["-listen", "0.0.0.0:8432", "-data", "/data", "-syslog-listen", ":514", "-trap-listen", ":162"]

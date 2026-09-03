# TopoLight build helpers. The binary is standard-library only; nothing to fetch.
VERSION ?= $(shell sed -n 's/.*Version *= *"\(.*\)".*/\1/p' internal/version/version.go)
LDFLAGS  = -s -w
# Paid builds pass ISSUER_PUBKEY=<base64 ed25519 public key>; the public repo
# builds without it and is therefore the Free edition.
ifneq ($(ISSUER_PUBKEY),)
LDFLAGS += -X github.com/nizartuanku/topolight/internal/license.IssuerPublicKey=$(ISSUER_PUBKEY)
endif
TARGETS = linux/amd64 linux/arm64 darwin/arm64 darwin/amd64 windows/amd64

.PHONY: build test vet fmt dist clean run

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o topolight ./cmd/topolight

run: build
	./topolight -data ./.data -listen 127.0.0.1:8433 -syslog-listen :5514 -trap-listen :1162

fmt:
	gofmt -l -w .

vet:
	go vet ./...

test:
	go test -race ./...

# dist/topolight_<ver>_<os>_<arch>.tar.gz (zip for windows) + dist/SHA256SUMS
DIST ?= dist
dist: vet test
	rm -rf $(DIST) && mkdir -p $(DIST)
	@for t in $(TARGETS); do \
	  os=$${t%/*}; arch=$${t#*/}; ext=""; [ $$os = windows ] && ext=.exe; \
	  d=$(DIST)/topolight_$(VERSION)_$${os}_$${arch}; mkdir -p $$d; \
	  echo "building $$t"; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o $$d/topolight$$ext ./cmd/topolight || exit 1; \
	  cp README.md LICENSE CHANGELOG.md $$d/; cp -r docs $$d/docs; rm -rf $$d/docs/img; \
	  [ $$os = linux ] && cp install.sh deploy/topolight.service $$d/ || true; \
	  if [ $$os = windows ]; then (cd $(DIST) && zip -qr $$(basename $$d).zip $$(basename $$d)); else (cd $(DIST) && tar -czf $$(basename $$d).tar.gz $$(basename $$d)); fi; \
	  rm -rf $$d; \
	done
	cd $(DIST) && sha256sum * > SHA256SUMS && cat SHA256SUMS

clean:
	rm -rf topolight dist .data

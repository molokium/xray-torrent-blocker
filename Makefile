BINARY  := tblocker
VERSION ?= v0.3.1-pg.10
LDFLAGS := -s -w -X main.Version=$(VERSION)
GOFLAGS := -trimpath

.PHONY: all build release clean test vet fmt check

all: build

build:
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o build/$(BINARY) .

release: clean
	mkdir -p dist/stage-amd64 dist/stage-arm64
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o dist/stage-amd64/$(BINARY) .
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o dist/stage-arm64/$(BINARY) .
	for arch in amd64 arm64; do \
		cp config.yaml.example tblocker.service install.sh README.md QA.md LICENSE dist/stage-$$arch/ ; \
		tar -C dist/stage-$$arch -czf dist/$(BINARY)-$(VERSION)-linux-$$arch.tar.gz . ; \
	done
	tar --exclude=.git --exclude=dist --exclude=build -czf dist/$(BINARY)-$(VERSION)-source.tar.gz .
	cd dist && sha256sum *.tar.gz > SHA256SUMS

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

check:
	@test -z "$$(gofmt -l .)" || (echo "gofmt required:"; gofmt -l .; exit 1)
	go vet ./...
	go test -race ./...

clean:
	rm -rf build dist

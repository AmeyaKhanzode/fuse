BINARY := mini_unionfs
GO     ?= go

.PHONY: all build deps test clean fmt vet

all: build

deps:
	$(GO) mod tidy

build: deps
	$(GO) build -o $(BINARY) .

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

test: build
	chmod +x ./test_unionfs.sh
	./test_unionfs.sh

clean:
	rm -f $(BINARY)
	rm -rf unionfs_test_env

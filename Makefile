#
# Author: Markus Stenberg <fingon@iki.fi>
#
# Copyright (c) 2024 Markus Stenberg
#
#

BINARY=bhugo
TEMPLATES = $(wildcard *.templ)
GENERATED = $(patsubst %.templ,%_templ.go,$(TEMPLATES))

all: build lint

build: $(BINARY)

lint:
	golangci-lint run --fix

$(BINARY): $(wildcard */*.go) $(wildcard *.go) $(GENERATED) Makefile
	go test ./...
	go build .

.PHONY: clean
clean:
	rm -f *_templ.go *_templ.txt $(BINARY)

upgrade:
	go get -u ./...
	go mod tidy

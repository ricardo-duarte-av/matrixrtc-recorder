export GOTOOLCHAIN := go1.26.8
TAGS := goolm

.PHONY: build test vet

build:
	go build -tags $(TAGS) -o matrixrtc-recorder ./cmd/matrixrtc-recorder

test:
	go test -tags $(TAGS) -race ./...

vet:
	go vet -tags $(TAGS) ./...

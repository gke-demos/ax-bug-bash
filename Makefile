SHELL := /usr/bin/env bash
export PATH := /usr/local/google/home/mofi/.local/go/bin:$(PATH)

KUBE_CONTEXT ?= $(shell kubectl config current-context 2>/dev/null || echo current-gke-context)
PROJECT_ID ?= $(shell gcloud config get-value project 2>/dev/null || echo sample-cloud-project)
REGION ?= us-central1
IMAGE_REGISTRY ?= $(REGION)-docker.pkg.dev/$(PROJECT_ID)/ax-bug-bash

.PHONY: build test eval run-local images deploy verify-manifests

build:
	mkdir -p bin
	go build -o bin/ax-observer ./cmd/ax-observer
	go build -o bin/storefront ./cmd/storefront
	go build -o bin/model-gateway ./cmd/model-gateway
	go build -o bin/ax-bug-bash-runner ./cmd/ax-bug-bash-runner

test:
	./services/run-service-tests.sh --all
	go test -v ./...

eval:
	go test -v ./internal/evals/...

run-local:
	./hack/run-local.sh

images:
	mkdir -p bin/linux_amd64
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/linux_amd64/ax-task-runner ./cmd/ax-bug-bash-runner
	docker build -f harnesses/Dockerfile.task-runner -t $(IMAGE_REGISTRY)/ax-task-runner:latest .
	docker push $(IMAGE_REGISTRY)/ax-task-runner:latest

deploy:
	KUBE_CONTEXT="$(KUBE_CONTEXT)" PROJECT_ID="$(PROJECT_ID)" IMAGE_REGISTRY="$(IMAGE_REGISTRY)" ./hack/deploy-gke.sh

verify-manifests:
	./hack/deploy-gke.sh --dry-run
	go test -v -run TestRepositoryHygieneAndManifestContract ./internal/evals/...

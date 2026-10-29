.PHONY: build test eval eval-live-gke run-local images deploy verify-manifests record-demo clean

KUBE_CONTEXT ?= $(shell kubectl config current-context 2>/dev/null || echo "gke-default-context")
PROJECT_ID   ?= $(shell gcloud config get-value project 2>/dev/null || echo "example-gcp-project")
REGION       ?= us-central1
AR_REPO      ?= ax-bug-bash
REGISTRY     ?= $(REGION)-docker.pkg.dev/$(PROJECT_ID)/$(AR_REPO)
TAG          ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "v060")

build:
	@mkdir -p bin
	go build -o bin/ax-observer ./cmd/ax-observer
	go build -o bin/model-gateway ./cmd/model-gateway
	go build -o bin/ax-task-runner ./cmd/ax-bug-bash-runner
	go build -o bin/polyglot-supervisor ./cmd/polyglot-supervisor
	go build -o bin/storefront-git ./cmd/storefront-git

test:
	go test -p 1 -v ./...
	bash services/run-service-tests.sh --all

eval:
	go test -p 1 -v -count=1 ./internal/evals/...

eval-live-gke:
	AX_LIVE_GKE_EVAL=1 go test -p 1 -v -count=1 -timeout=15m -run TestLiveGKEFourByFourChaosSoakAndDualExport ./internal/evals/...

run-local:
	./hack/run-local.sh

images: build
	@mkdir -p bin/linux_amd64
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/linux_amd64/ax-observer ./cmd/ax-observer
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/linux_amd64/model-gateway ./cmd/model-gateway
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/linux_amd64/ax-task-runner ./cmd/ax-bug-bash-runner
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/linux_amd64/polyglot-supervisor ./cmd/polyglot-supervisor
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/linux_amd64/storefront-git ./cmd/storefront-git
	docker build -f deploy/model-gateway/Dockerfile -t $(REGISTRY)/model-gateway:$(TAG) .
	docker build -f Dockerfile.polyglot-mesh -t $(REGISTRY)/storefront:$(TAG) .
	docker build -f deploy/ax-observer/Dockerfile -t $(REGISTRY)/ax-observer:$(TAG) .
	docker build -f harnesses/Dockerfile.task-runner -t $(REGISTRY)/ax-task-runner:$(TAG) .
	IMAGE_URI=$(REGISTRY)/ax-server:$(TAG) ./deploy/ax-server/build.sh

deploy:
	./hack/deploy-gke.sh

verify-manifests:
	./hack/deploy-gke.sh --dry-run
	go test -p 1 -v -run TestRepositoryHygieneAndManifestContract ./internal/evals/...

record-demo:
	python3 ./hack/record-terminal-demo.py
	./hack/record-stage-walkthrough.sh

clean:
	rm -rf bin /tmp/ax-bug-bash-* /tmp/ax-live-gke-eval-report.json

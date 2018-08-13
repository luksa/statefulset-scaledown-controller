VERSION       ?= $(shell git describe --always --abbrev=7 --dirty)
REGISTRY      ?= docker.io/luksa/

build:
	CGO_ENABLED=0 GOOS=linux go build cmd/controller/controller.go

image: build
	docker build -t $(REGISTRY)statefulset-scaledown-controller:$(VERSION) .

push: image
	docker push $(REGISTRY)statefulset-scaledown-controller:$(VERSION)

run: build
	./statefulset-scaledown-controller --kubeconfig ~/.kube/config --alsologtostderr --v 4

deploy:
	kubectl apply -f artifacts/cluster-scoped.yaml

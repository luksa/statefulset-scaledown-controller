build:
	CGO_ENABLED=0 GOOS=linux go build cmd/controller/controller.go

image: build
	docker build -t docker.io/luksa/statefulset-drain-controller:latest .

push: image
	docker push docker.io/luksa/statefulset-drain-controller:latest

run: build
	./statefulset-drain-controller --kubeconfig ~/.kube/config --alsologtostderr --v 4

deploy:
	kubectl apply -f artifacts/kubernetes.yaml

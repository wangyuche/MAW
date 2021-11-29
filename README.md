```shell
install cert manager
kubectl apply -f https://github.com/jetstack/cert-manager/releases/download/v1.2.0/cert-manager.yaml
kubectl apply -f https://github.com/jetstack/cert-manager/releases/download/v1.2.0/cert-manager.crds.yaml

using minikube
eval $(minikube docker-env)
make build file=aaa ver=0.0.0
make deploy
kubectl apply -f test.yaml 
```
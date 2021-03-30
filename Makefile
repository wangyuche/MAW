build:
	GOOS=linux GOARCH=amd64 go build -o ${file}_${ver}
	cp -r ${file}_${ver} dockerfile
	cd dockerfile && \
	sh build.sh ${file} ${ver}

deploy:
	kubectl apply -f deploy.yaml
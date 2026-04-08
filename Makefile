.PHONY: build build-worker build-artificial run-artificial run-worker clean

BIN := bin
PORT ?= 4000

build: build-artificial build-worker

build-artificial:
	go build -o $(BIN)/svc-artificial ./src/svc-artificial/cmd/artificial/

build-worker:
	go build -o $(BIN)/cmd-worker ./src/cmd-worker/cmd/worker/

run-artificial: build
	$(BIN)/svc-artificial --port $(PORT) --worker-bin $(CURDIR)/$(BIN)/cmd-worker

run-worker: build-worker
	@test -n "$(EMPLOYEE_ID)" || (echo "usage: make run-worker EMPLOYEE_ID=2" && exit 1)
	$(BIN)/cmd-worker --server localhost:$(PORT) --employee-id $(EMPLOYEE_ID)

clean:
	rm -rf $(BIN)

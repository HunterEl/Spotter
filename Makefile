BINARY=spotter

VERSION=0.0.1
BUILD=`git rev-parse HEAD`

LDFLAGS=-ldflags "-X main.version=$(VERSION) -X main.build=$(BUILD)"

.DEFAULT_GOAL: $(BINARY)

$(BINARY):
	go build ${LDFLAGS} -o ${BINARY} spotter.go

install:
	go install ${LDFLAGS} ./*.go

builddocker:
	docker build -t hunterel/spotter .

clean:
	if [ -f ${BINARY} ] ; then rm ${BINARY} ; fi

.PHONY: clean install
